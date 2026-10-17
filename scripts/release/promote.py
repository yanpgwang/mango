"""Promote a reviewed main-branch candidate; never rebuild release payloads."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import zipfile

from build import PLATFORMS, VERSION_PATTERN
from images import inspect_oci
from smoke import read_candidate

REPOSITORY = "yanpgwang/mango"


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def github(path, method="GET", body=None):
    args = ["gh", "api", f"repos/{REPOSITORY}/{path}", "--method", method]
    if body is None:
        return json.loads(command(*args))
    return json.loads(subprocess.check_output(
        [*args, "--input", "-"], input=json.dumps(body), text=True))


def fetch(url):
    try:
        with urllib.request.urlopen(url, timeout=60) as response:
            return response.read()
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return None
        raise


def identity(version, revision):
    match = VERSION_PATTERN.fullmatch(version)
    if not match or not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("expected canonical alpha version and full source revision")
    return ".".join(match.group(1, 2, 3)) + "a" + match.group(4)


def accepted_run(run_id, version, revision):
    identity(version, revision)
    run = github(f"actions/runs/{run_id}")
    if (run.get("path") != ".github/workflows/release-build.yml"
            or run.get("event") != "workflow_dispatch"
            or run.get("head_branch") != "main"
            or run.get("head_sha") != revision
            or run.get("status") != "completed" or run.get("conclusion") != "success"
            or run.get("repository", {}).get("full_name") != REPOSITORY):
        raise ValueError("candidate must be a successful main-branch release-build run at the requested source")
    subprocess.run(["git", "merge-base", "--is-ancestor", revision, "origin/main"], check=True)
    for workflow in ("ci.yml", "integration.yml", "pages.yml"):
        query = urllib.parse.urlencode({"head_sha": revision, "event": "push", "branch": "main", "per_page": 100})
        runs = github(f"actions/workflows/{workflow}/runs?{query}")["workflow_runs"]
        # A later failed rerun supersedes earlier success. Never cherry-pick green.
        if not runs or runs[0]["status"] != "completed" or runs[0]["conclusion"] != "success":
            raise ValueError(f"source lacks successful current main CI: {workflow}")
    return run


def validate(folder, version, revision):
    python_version = identity(version, revision)
    for name in ("manifest.json", "SHA256SUMS"):
        if not (folder / name).is_file() or (folder / name).is_symlink():
            raise ValueError("candidate metadata must be an ordinary file")
    manifest = read_candidate(folder)
    if (manifest.get("version") != version or manifest.get("revision") != revision
            or manifest.get("sdk_versions") != {"go": version, "typescript": version, "python": python_version}):
        raise ValueError("candidate identity differs from the requested source/version")
    expected = {f"mango-{version}-{platform.replace('/', '-')}.tar.gz": ("commands", platform)
                for platform in PLATFORMS}
    expected.update({
        f"mango-sdk-go-{version}.zip": ("go-sdk", None),
        f"mango_sdk-{python_version}-py3-none-any.whl": ("python-sdk", None),
        f"mango_sdk-{python_version}.tar.gz": ("python-sdk", None),
        f"mango-sdk-{version}.tgz": ("typescript-sdk", None),
        f"mango-{version}.tgz": ("helm-chart", None),
        "mango-oci.tar": ("oci-image", None),
        "mango-self-hosted-worker-oci.tar": ("oci-image", None),
    })
    records = manifest["artifacts"]
    if {r["name"] for r in records} != set(expected) or len(records) != len(expected):
        raise ValueError("candidate payload inventory differs from the release allowlist")
    if {p.name for p in folder.iterdir()} != set(expected) | {"manifest.json", "SHA256SUMS"}:
        raise ValueError("unexpected file in candidate directory")
    for record in records:
        if (record["kind"], record.get("platform")) != expected[record["name"]]:
            raise ValueError("candidate artifact kind/platform mismatch")
        if not (folder / record["name"]).is_file() or (folder / record["name"]).is_symlink():
            raise ValueError("candidate payload must be an ordinary file")
        if record["kind"] == "oci-image":
            image = record["name"].removesuffix("-oci.tar")
            if record.get("image") != image:
                raise ValueError("OCI destination does not match candidate archive")
            inspected = inspect_oci(folder / record["name"], version, revision)
            if any(record.get(key) != value for key, value in inspected.items()):
                raise ValueError("OCI digest/platform record differs from inspected archive")
    return manifest


def verify_download(url, record, host):
    if urllib.parse.urlsplit(url).hostname != host:
        raise ValueError("unexpected registry artifact host")
    data = fetch(url)
    if data is None or len(data) != record["size_bytes"] or hashlib.sha256(data).hexdigest() != record["sha256"]:
        raise ValueError(f"public registry bytes conflict: {record['name']}")


def registry_state(manifest):
    """Verify existing bytes before any upload; return only missing packages."""
    version, python_version = manifest["version"], manifest["sdk_versions"]["python"]
    npm_record = next(r for r in manifest["artifacts"] if r["kind"] == "typescript-sdk")
    npm_body = fetch(f"https://registry.npmjs.org/mango-sdk/{version}")
    if npm_body is not None:
        npm = json.loads(npm_body)
        if npm.get("name") != "mango-sdk" or npm.get("version") != version:
            raise ValueError("unexpected npm identity")
        verify_download(npm["dist"]["tarball"], npm_record, "registry.npmjs.org")
    python_records = {r["name"]: r for r in manifest["artifacts"] if r["kind"] == "python-sdk"}
    python_body = fetch(f"https://pypi.org/pypi/mango-sdk/{python_version}/json")
    present = set()
    if python_body is not None:
        for entry in json.loads(python_body)["urls"]:
            name = entry["filename"]
            if name not in python_records or name in present or entry.get("yanked"):
                raise ValueError("unexpected or yanked PyPI distribution")
            verify_download(entry["url"], python_records[name], "files.pythonhosted.org")
            present.add(name)
    return npm_body is None, sorted(set(python_records) - present)


def ensure_tag(tag, revision, create=True):
    # Lightweight tags created by this workflow have an unambiguous exact target.
    result = subprocess.run(["gh", "api", f"repos/{REPOSITORY}/git/ref/tags/{tag}"],
                            capture_output=True, text=True)
    if result.returncode:
        if "HTTP 404" not in result.stderr:
            raise ValueError("cannot inspect existing release tag")
        if create:
            github("git/refs", "POST", {"ref": f"refs/tags/{tag}", "sha": revision})
    else:
        ref = json.loads(result.stdout)
        if ref["object"]["type"] != "commit" or ref["object"]["sha"] != revision:
            raise ValueError(f"existing tag conflicts with source: {tag}")


def prepare(args):
    accepted_run(args.run, args.version, args.revision)
    if args.folder.exists():
        raise ValueError("download destination must not already exist")
    subprocess.run(["gh", "run", "download", str(args.run), "--repo", REPOSITORY,
                    "--name", f"mango-candidate-{args.version}-{args.revision}",
                    "--dir", str(args.folder)], check=True)
    validate(args.folder, args.version, args.revision)
    print("Validated accepted candidate source, checks, inventory, hashes and OCI identity")


def stage(args):
    manifest = validate(args.folder, args.version, args.revision)
    accepted_run(args.run, args.version, args.revision)
    missing_npm, missing_python = registry_state(manifest)
    stage_folder = args.folder.parent / "pypi-upload"
    stage_folder.mkdir()
    for name in missing_python:
        shutil.copyfile(args.folder / name, stage_folder / name)
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write(f"npm_missing={str(missing_npm).lower()}\n")
        output.write(f"python_missing={str(bool(missing_python)).lower()}\n")
    # Detect conflicts before any publication; create tags only after images
    # are publicly retrievable and native SDK distributions have been verified.
    ensure_tag("v" + args.version, args.revision, create=False)
    ensure_tag("sdk/go/v" + args.version, args.revision, create=False)


def images(args):
    manifest = validate(args.folder, args.version, args.revision)
    records = [r for r in manifest["artifacts"] if r["kind"] == "oci-image"]
    repositories = {r["image"]: f"ghcr.io/yanpgwang/{r['image']}" for r in records}
    # Digest destinations cannot overwrite a different image/version. This also
    # bootstraps new GHCR packages, which start private, without interpreting
    # an authorization error as an absent version tag. Bootstrap both first.
    for record in records:
        destination = repositories[record["image"]] + "@" + record["digest"]
        subprocess.run(["skopeo", "copy", "--all", "--preserve-digests",
                        "oci-archive:" + str(args.folder / record["name"]), "docker://" + destination], check=True)
    for record in records:
        destination = repositories[record["image"]] + "@" + record["digest"]
        verify_public_image(destination, record["digest"])
    # A publicly visible package now has an inspectable version namespace.
    for record in records:
        destination = repositories[record["image"]] + ":" + args.version
        existing = subprocess.run(["skopeo", "inspect", "--raw", "docker://" + destination],
                                  capture_output=True)
        if existing.returncode == 0:
            if "sha256:" + hashlib.sha256(existing.stdout).hexdigest() != record["digest"]:
                raise ValueError("existing image tag conflicts with candidate digest")
        elif not any(code in existing.stderr.lower() for code in (b"manifest unknown", b"name unknown")):
            raise ValueError("cannot establish image destination state; check GHCR access")
        subprocess.run(["skopeo", "copy", "--all", "--preserve-digests",
                        "oci-archive:" + str(args.folder / record["name"]), "docker://" + destination], check=True)
        verify_public_image(destination, record["digest"])


def verify_public_image(destination, digest):
    # Empty authfile makes public visibility a real gate, not a token-assisted read.
    with tempfile.NamedTemporaryFile(mode="w") as auth:
        auth.write('{"auths":{}}')
        auth.flush()
        try:
            raw = subprocess.check_output(["skopeo", "inspect", "--authfile", auth.name,
                                           "--raw", "docker://" + destination], stderr=subprocess.DEVNULL)
        except subprocess.CalledProcessError as error:
            raise ValueError("image is not publicly retrievable; make both Mango GHCR packages public, "
                             "then retry this exact candidate before uploading SDKs") from error
    if "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
        raise ValueError("public image index digest differs from accepted candidate")


def finish(args):
    manifest = validate(args.folder, args.version, args.revision)
    accepted_run(args.run, args.version, args.revision)
    missing_npm, missing_python = registry_state(manifest)
    if missing_npm or missing_python:
        raise ValueError("public SDK distributions remain missing")
    tag = "v" + args.version
    ensure_tag(tag, args.revision)
    ensure_tag("sdk/go/" + tag, args.revision)
    # Retrieval through the public module transport must expose the same Go files.
    with tempfile.TemporaryDirectory(prefix="mango-public-go-") as temporary:
        env = {**os.environ, "GOPATH": temporary, "GOPROXY": "https://proxy.golang.org,direct", "GOPRIVATE": ""}
        module = json.loads(subprocess.check_output(
            ["go", "mod", "download", "-json", f"github.com/{REPOSITORY}/sdk/go@v{args.version}"], env=env, text=True))
        record = next(r for r in manifest["artifacts"] if r["kind"] == "go-sdk")
        with zipfile.ZipFile(args.folder / record["name"]) as accepted, zipfile.ZipFile(module["Zip"]) as public:
            for entry in accepted.namelist():
                relative = entry.split("/", 1)[1]
                if public.read(f"github.com/{REPOSITORY}/sdk/go@v{args.version}/" + relative) != accepted.read(entry):
                    raise ValueError("published Go module differs from inspected SDK")
    result = subprocess.run(["gh", "api", f"repos/{REPOSITORY}/releases/tags/{tag}"], capture_output=True, text=True)
    release = None
    if result.returncode == 0:
        release = json.loads(result.stdout)
        if release.get("target_commitish") != args.revision or not release.get("prerelease"):
            raise ValueError("existing release identity conflicts with candidate")
    elif "HTTP 404" not in result.stderr:
        raise ValueError("cannot inspect release destination")
    image_lines = [f"- `ghcr.io/yanpgwang/{r['image']}@{r['digest']}`" for r in manifest["artifacts"] if r["kind"] == "oci-image"]
    body = (f"Developer preview from `{args.revision}`.\n\n"
            f"Accepted candidate: https://github.com/{REPOSITORY}/actions/runs/{args.run}\n\n"
            f"Promotion: https://github.com/{REPOSITORY}/actions/runs/{os.environ['GITHUB_RUN_ID']}\n\n"
            "Supports fresh installation, Mango process replacement and Environment Work recovery. "
            "External Temporal database cold restore remains experimental after an unexplained CI failure; "
            "no production HA, upgrade or rollback promise. Internal content transport is bounded at 1 MiB; "
            "capacity failures preserve committed history and usage but can discard uncommitted output.\n\n"
            "Install the attached chart with operator-provided state services, Secrets and external Docker worker. "
            f"See https://github.com/{REPOSITORY}/blob/{args.revision}/docs/guides/kubernetes.md\n\n"
            + "\n".join(image_lines) + "\n\nCheck `SHA256SUMS` before installing archives. "
            "All archives and OCI indexes are promoted unchanged; checksums are not a signed build attestation.")
    if release is None:
        release = github("releases", "POST", {"tag_name": tag, "target_commitish": args.revision,
            "name": "Mango " + tag, "body": body, "draft": True, "prerelease": True})
    expected = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in args.folder.iterdir()}
    for asset in release["assets"]:
        if asset["name"] not in expected or asset.get("digest") != "sha256:" + expected[asset["name"]]:
            raise ValueError("existing release asset differs from accepted bytes")
    present = {a["name"] for a in release["assets"]}
    missing = [str(args.folder / name) for name in sorted(set(expected) - present)]
    if missing:
        if not release["draft"]:
            raise ValueError("public release is incomplete; refusing to modify its assets")
        subprocess.run(["gh", "release", "upload", tag, *missing, "--repo", REPOSITORY], check=True)
    uploaded = github(f"releases/{release['id']}")
    if {a["name"]: a.get("digest") for a in uploaded["assets"]} != {
            name: "sha256:" + digest for name, digest in expected.items()}:
        raise ValueError("uploaded release inventory or digests differ from accepted candidate")
    github(f"releases/{release['id']}", "PATCH", {"body": body, "draft": False, "prerelease": True, "make_latest": "false"})
    # Download without GitHub authorization, checking actual public bytes.
    for name, digest in expected.items():
        data = fetch(f"https://github.com/{REPOSITORY}/releases/download/{tag}/{name}")
        if data is None or hashlib.sha256(data).hexdigest() != digest:
            raise ValueError("public release retrieval differs from candidate")
    print(f"Verified public developer preview: https://github.com/{REPOSITORY}/releases/tag/{tag}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("prepare", "stage", "images", "finish"))
    parser.add_argument("--run", required=True, type=int)
    parser.add_argument("--version", required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--folder", required=True, type=Path)
    args = parser.parse_args()
    if args.run <= 0:
        parser.error("candidate run must be positive")
    globals()[args.operation](args)


if __name__ == "__main__":
    main()
