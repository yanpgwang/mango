"""Build matched release candidates from clean source; never publish registries."""

import argparse
import ctypes
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import tomllib
import zipfile


PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")
VERSION_PATTERN = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-alpha\.(0|[1-9][0-9]*)")


def git(root, *arguments):
    return subprocess.check_output(["git", "-C", str(root), *arguments], text=True).strip()


def validate_source(root, version, output, revision=None):
    match = VERSION_PATTERN.fullmatch(version)
    if not match:
        raise ValueError("version must be canonical X.Y.Z-alpha.N")
    if os.path.lexists(output):
        raise ValueError(f"candidate destination already exists: {output}")
    python_version = ".".join(match.group(1, 2, 3)) + "a" + match.group(4)
    typescript = json.loads((root / "sdk/typescript/package.json").read_text())
    if typescript.get("name") != "mango-sdk" or typescript.get("version") != version:
        raise ValueError("TypeScript SDK name/version does not match the candidate")
    python = tomllib.loads((root / "sdk/python/pyproject.toml").read_text())["project"]
    python_source = (root / "sdk/python/src/mango_sdk/_version.py").read_text()
    if python.get("name") != "mango-sdk" or python.get("version") != python_version or not re.search(rf'^__version__ = "{re.escape(python_version)}"$', python_source, re.MULTILINE):
        raise ValueError("Python SDK metadata/source version does not match the candidate")
    go_source = (root / "sdk/go/version.go").read_text()
    if not re.search(rf'^const Version = "{re.escape(version)}"$', go_source, re.MULTILINE):
        raise ValueError("Go SDK source version does not match the candidate")
    actual_revision = git(root, "rev-parse", "HEAD")
    if revision is not None and revision != actual_revision:
        raise ValueError("revision must match the checked-out Git commit")
    if git(root, "status", "--porcelain", "--untracked-files=normal"):
        raise ValueError("release candidates require a clean source checkout")
    return actual_revision, {"go": version, "typescript": version, "python": python_version}


def publish(stage, output):
    """Atomically rename without replacing even an existing empty directory."""
    libc = ctypes.CDLL(None, use_errno=True)
    if sys.platform == "darwin":
        rename = libc.renamex_np
        rename.argtypes = (ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint)
        rename.restype = ctypes.c_int
        result = rename(os.fsencode(stage), os.fsencode(output), 4)  # RENAME_EXCL
    elif sys.platform.startswith("linux"):
        rename = libc.renameat2
        rename.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
        rename.restype = ctypes.c_int
        result = rename(-100, os.fsencode(stage), -100, os.fsencode(output), 1)  # RENAME_NOREPLACE
    else:
        raise ValueError("release candidate building supports Linux and macOS")
    if result != 0:
        number = ctypes.get_errno()
        raise OSError(number, os.strerror(number), str(output))


def snapshot_source(root, destination, revision):
    archive = subprocess.check_output(["git", "-C", str(root), "archive", "--format=tar", revision])
    destination.mkdir()
    with tarfile.open(fileobj=io.BytesIO(archive)) as package:
        # Only ordinary tracked files/directories are build inputs. Never follow
        # a committed symlink into local operator files outside this snapshot.
        if any(not (member.isfile() or member.isdir()) for member in package.getmembers()):
            raise ValueError("release source snapshot contains a symbolic link or special file")
        package.extractall(destination, filter="data")


def pack_binaries(archive, binaries, license_path, prefix):
    if set(binaries) != {"mango", "mango-worker"}:
        raise ValueError("a command archive must contain both runtime commands")
    with archive.open("wb") as raw, gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as package:
            for name, path in [*binaries.items(), ("LICENSE", license_path)]:
                if path.is_symlink():
                    raise ValueError("distribution inputs must not be symbolic links")
                content = path.read_bytes()
                header = tarfile.TarInfo(f"{prefix}/{name}")
                header.size = len(content)
                header.mode = 0o644 if name == "LICENSE" else 0o755
                package.addfile(header, io.BytesIO(content))


def pack_go_sdk(root, archive, version):
    source = root / "sdk/go"
    with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as package:
        for path in sorted(source.rglob("*")):
            if path.is_dir():
                continue
            relative = path.relative_to(source)
            if relative.parts[0] in {"examples", "tests"}:
                continue
            if relative.name not in {"go.mod", "go.sum", "LICENSE", "README.md"} and (relative.suffix != ".go" or relative.name.endswith("_test.go")):
                continue
            if path.is_symlink():
                raise ValueError("Go SDK distribution inputs must not be symbolic links")
            info = zipfile.ZipInfo(f"mango-sdk-go-{version}/{relative.as_posix()}")
            info.external_attr = 0o100644 << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            package.writestr(info, path.read_bytes())


def write_manifest(stage, version, revision, sdk_versions, artifacts):
    records = []
    for artifact in sorted(artifacts, key=lambda item: item["name"]):
        content = (stage / artifact["name"]).read_bytes()
        records.append({**artifact, "size_bytes": len(content), "sha256": hashlib.sha256(content).hexdigest()})
    manifest = {"schema_version": 1, "version": version, "revision": revision, "sdk_versions": sdk_versions, "artifacts": records}
    encoded = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
    (stage / "manifest.json").write_bytes(encoded)
    checksums = {item["name"]: item["sha256"] for item in records}
    checksums["manifest.json"] = hashlib.sha256(encoded).hexdigest()
    (stage / "SHA256SUMS").write_text("".join(f"{digest}  {name}\n" for name, digest in sorted(checksums.items())))


def run(arguments, root, env=None):
    subprocess.run(arguments, cwd=root, env=env, check=True)


def build_payloads(root, stage, version, platforms, revision):
    artifacts = []
    flags = f"-s -w -X github.com/yanpgwang/mango/internal/buildinfo.Version={version} -X github.com/yanpgwang/mango/internal/buildinfo.Revision={revision}"
    for platform in platforms:
        target_os, target_arch = platform.split("/")
        with tempfile.TemporaryDirectory(prefix="mango-commands-") as folder:
            binaries = {}
            for command in ("mango", "mango-worker"):
                binaries[command] = Path(folder) / command
                env = {**os.environ, "CGO_ENABLED": "0", "GOOS": target_os, "GOARCH": target_arch}
                run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags", flags, "-o", str(binaries[command]), f"./cmd/{command}"], root, env)
            name = f"mango-{version}-{target_os}-{target_arch}.tar.gz"
            pack_binaries(stage / name, binaries, root / "LICENSE", name.removesuffix(".tar.gz"))
            artifacts.append({"name": name, "kind": "commands", "platform": platform})
    go_name = f"mango-sdk-go-{version}.zip"
    pack_go_sdk(root, stage / go_name, version)
    artifacts.append({"name": go_name, "kind": "go-sdk"})
    with tempfile.TemporaryDirectory(prefix="mango-python-") as folder:
        run(["uv", "build", "--project", str(root / "sdk/python"), "--no-sources", "--out-dir", folder], root)
        python_version = tomllib.loads((root / "sdk/python/pyproject.toml").read_text())["project"]["version"]
        expected = {f"mango_sdk-{python_version}-py3-none-any.whl", f"mango_sdk-{python_version}.tar.gz"}
        if {path.name for path in Path(folder).iterdir()} != expected:
            raise ValueError("Python build did not produce exactly the expected wheel and sdist")
        for name in sorted(expected):
            shutil.copyfile(Path(folder) / name, stage / name)
            artifacts.append({"name": name, "kind": "python-sdk"})
    run(["npm", "ci"], root / "sdk/typescript")
    run(["npm", "pack", "--pack-destination", str(stage)], root / "sdk/typescript")
    name = f"mango-sdk-{version}.tgz"
    if not (stage / name).is_file():
        raise ValueError("npm did not produce the expected package")
    artifacts.append({"name": name, "kind": "typescript-sdk"})
    return artifacts


def build(root, output, version, platforms, revision=None):
    root, output = root.resolve(), output.absolute()
    if not platforms or len(set(platforms)) != len(platforms) or any(platform not in PLATFORMS for platform in platforms):
        raise ValueError("platforms must be unique supported OS/architecture pairs")
    revision, sdk_versions = validate_source(root, version, output, revision)
    output.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}-", dir=output.parent))
    try:
        with tempfile.TemporaryDirectory(prefix="mango-release-source-") as temporary:
            snapshot = Path(temporary) / "source"
            snapshot_source(root, snapshot, revision)
            artifacts = build_payloads(snapshot, stage, version, platforms, revision)
        # A source edit or commit during a long build must never publish mixed bytes.
        validate_source(root, version, output, revision)
        write_manifest(stage, version, revision, sdk_versions, artifacts)
        publish(stage, output)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    print(f"Release candidate {version} ({revision}): {output}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--platform", action="append", choices=PLATFORMS)
    parser.add_argument("--revision")
    args = parser.parse_args()
    try:
        build(Path(__file__).resolve().parents[2], args.output, args.version, args.platform or list(PLATFORMS), args.revision)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"release candidate failed: {error}\n")


if __name__ == "__main__":
    main()
