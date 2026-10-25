"""Inspect candidates and install their SDKs outside the source checkout."""

import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path, PurePosixPath
import platform
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import zipfile

from build import validate_chart_metadata


def read_candidate(folder):
    manifest_bytes = (folder / "manifest.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    if manifest.get("schema_version") != 1 or not manifest.get("artifacts"):
        raise ValueError("invalid candidate manifest")
    names = set()
    for record in manifest["artifacts"]:
        name = record["name"]
        if not name or Path(name).name != name or name in names:
            raise ValueError("invalid or duplicate artifact name")
        names.add(name)
        content = (folder / name).read_bytes()
        if len(content) != record["size_bytes"] or hashlib.sha256(content).hexdigest() != record["sha256"]:
            raise ValueError(f"artifact checksum mismatch: {name}")
    checksums = {}
    for line in (folder / "SHA256SUMS").read_text().splitlines():
        digest, name = line.split("  ", 1)
        if name in checksums:
            raise ValueError("duplicate checksum entry")
        checksums[name] = digest
    expected = {record["name"]: record["sha256"] for record in manifest["artifacts"]}
    expected["manifest.json"] = hashlib.sha256(manifest_bytes).hexdigest()
    if checksums != expected:
        raise ValueError("candidate checksum index does not match the manifest")
    return manifest


def run(arguments, cwd, env=None):
    subprocess.run(arguments, cwd=cwd, env=env, check=True)


def inspect_commands(folder, manifest, destination):
    system = platform.system().lower()
    architecture = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    records = [record for record in manifest["artifacts"] if record["kind"] == "commands"]
    found_native = False
    for record in records:
        prefix = record["name"].removesuffix(".tar.gz")
        expected = {f"{prefix}/{name}" for name in ("mango", "mango-worker", "LICENSE")}
        with tarfile.open(folder / record["name"]) as package:
            members = package.getmembers()
            if {member.name for member in members} != expected or len(members) != 3 or any(not member.isfile() for member in members):
                raise ValueError("unexpected command archive contents")
            if record.get("platform") == f"{system}/{architecture}":
                found_native = True
                for name in ("mango", "mango-worker"):
                    target = destination / name
                    target.write_bytes(package.extractfile(f"{prefix}/{name}").read())
                    target.chmod(0o755)
                    identity = json.loads(subprocess.check_output([str(target), "version"], text=True))
                    if identity != {"version": manifest["version"], "revision": manifest["revision"]}:
                        raise ValueError(f"incorrect linked identity: {name}")
    if not found_native:
        raise ValueError("candidate lacks a command archive for this verification host")


class Fixture(BaseHTTPRequestHandler):
    """A simulated HTTP boundary for package installation checks, not Mango."""
    failures = []
    calls = 0

    def do_GET(self):
        if self.path != "/v1/agents?limit=1" or self.headers.get("Authorization") != "Bearer release-fixture-key":
            self.failures.append("installed client encoded an unexpected request")
            self.send_error(400)
            return
        type(self).calls += 1
        body = b'{"data":[],"next_page":null}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def inspect_python(folder, manifest, destination, url):
    records = [record for record in manifest["artifacts"] if record["kind"] == "python-sdk"]
    if len(records) != 2 or not any(record["name"].endswith(".whl") for record in records) or not any(record["name"].endswith(".tar.gz") for record in records):
        raise ValueError("candidate must contain a Python wheel and source distribution")
    for index, record in enumerate(records):
        environment = destination / f"python-{index}"
        run(["uv", "venv", "--python", sys.executable, str(environment)], destination)
        python = environment / "bin/python"
        run(["uv", "pip", "install", "--python", str(python), str(folder / record["name"])], destination)
        program = '''import asyncio, importlib.metadata, os, pathlib, sys
import mango_sdk
from mango_sdk import Mango, AsyncMango
assert pathlib.Path(mango_sdk.__file__).is_relative_to(sys.prefix)
assert mango_sdk.__version__ == importlib.metadata.version("mango-sdk") == os.environ["SDK_VERSION"]
with Mango(base_url=os.environ["FIXTURE_URL"], api_key="release-fixture-key") as client:
    assert client.agents.list(limit=1) == {"data": [], "next_page": None}
async def check():
    async with AsyncMango(base_url=os.environ["FIXTURE_URL"], api_key="release-fixture-key") as client:
        assert await client.agents.list(limit=1) == {"data": [], "next_page": None}
asyncio.run(check())
'''
        env = {**os.environ, "FIXTURE_URL": url, "SDK_VERSION": manifest["sdk_versions"]["python"]}
        env.pop("PYTHONPATH", None)
        run([str(python), "-I", "-c", program], destination, env)


def inspect_typescript(root, folder, manifest, destination, url):
    records = [record for record in manifest["artifacts"] if record["kind"] == "typescript-sdk"]
    if len(records) != 1:
        raise ValueError("candidate must contain exactly one npm package")
    app = destination / "typescript"
    app.mkdir()
    (app / "package.json").write_text('{"private":true,"type":"module"}')
    run(["npm", "install", "--ignore-scripts", "--no-audit", "--no-fund", str(folder / records[0]["name"]), "typescript@5.9.3"], app)
    metadata = json.loads((app / "node_modules/mango-sdk/package.json").read_text())
    if metadata["version"] != manifest["sdk_versions"]["typescript"]:
        raise ValueError("installed npm package has an incorrect version")
    (app / "check.mjs").write_text('''import assert from 'node:assert/strict';
import { Mango, APIError } from 'mango-sdk';
assert.equal(typeof APIError, 'function');
const client = new Mango({ baseURL: process.env.FIXTURE_URL, apiKey: 'release-fixture-key' });
assert.deepEqual(await client.agents.list({ limit: 1 }), { data: [], next_page: null });
''')
    run(["node", "check.mjs"], app, {**os.environ, "FIXTURE_URL": url})
    (app / "check.ts").write_text('''import { Mango } from 'mango-sdk';
const client = new Mango({ baseURL: 'http://localhost', apiKey: 'fixture' });
const page = await client.agents.list({ limit: 1 });
const count: number = page.data.length;
void count;
// @ts-expect-error unknown request fields must remain rejected by installed declarations.
await client.agents.list({ invented: true });
''')
    compiler = app / "node_modules/.bin/tsc"
    run([str(compiler), "--noEmit", "--strict", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "check.ts"], app)


def inspect_go(folder, manifest, destination, url):
    records = [record for record in manifest["artifacts"] if record["kind"] == "go-sdk"]
    if len(records) != 1:
        raise ValueError("candidate must contain exactly one Go SDK source archive")
    source = destination / "go-sdk"
    source.mkdir()
    prefix = f'mango-sdk-go-{manifest["sdk_versions"]["go"]}/'
    with zipfile.ZipFile(folder / records[0]["name"]) as package:
        for member in package.infolist():
            path = PurePosixPath(member.filename)
            if not member.filename.startswith(prefix) or path.is_absolute() or ".." in path.parts or stat.S_ISLNK(member.external_attr >> 16):
                raise ValueError("invalid Go SDK archive path")
            target = source / member.filename.removeprefix(prefix)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(package.read(member))
    app = destination / "go-app"
    app.mkdir()
    (app / "main.go").write_text('''package main
import ("context"; "os"; mango "github.com/yanpgwang/mango/sdk/go")
func main() {
    if mango.Version != os.Getenv("SDK_VERSION") { panic("incorrect Go SDK version") }
    client, err := mango.New(mango.Config{BaseURL:os.Getenv("FIXTURE_URL"), APIKey:"release-fixture-key"}); if err != nil { panic(err) }
    page, err := client.Agents.List(context.Background(), mango.ListAgentsParams{Limit:mango.Some(int64(1))}); if err != nil { panic(err) }
    if len(page.Data) != 0 { panic("incorrect decoded response") }
}
''')
    run(["go", "mod", "init", "example.invalid/release-smoke"], app)
    run(["go", "mod", "edit", "-require=github.com/yanpgwang/mango/sdk/go@v0.0.0", f"-replace=github.com/yanpgwang/mango/sdk/go={source}"], app)
    run(["go", "mod", "tidy"], app)
    run(["go", "run", "."], app, {**os.environ, "FIXTURE_URL": url, "SDK_VERSION": manifest["sdk_versions"]["go"]})


def inspect_chart(folder, manifest, destination, helm):
    records = [item for item in manifest["artifacts"] if item["kind"] == "helm-chart"]
    if len(records) != 1:
        raise ValueError("candidate must contain exactly one chart")
    chart = folder / records[0]["name"]
    metadata = subprocess.check_output([helm, "show", "chart", str(chart)], text=True)
    validate_chart_metadata(metadata, manifest["version"])
    # Independently authored operator inputs; no secrets or generated fixtures.
    values = destination / "chart-values.yaml"
    values.write_text('''database:
  existingSecret: {name: operator-db, key: url}
auth:
  existingSecret: {name: operator-auth, key: token}
temporal:
  address: temporal.operator.test:7233
nats:
  existingSecret: {name: operator-nats, key: url}
files:
  endpoint: http://s3.operator.test:8333
  bucket: operator-bucket
  existingSecret: {name: operator-s3}
model:
  baseURL: https://model.operator.test
  id: operator-model
  existingSecret: {name: operator-model, key: token}
''')
    run([helm, "lint", "--strict", str(chart), "--values", str(values)], destination)
    rendered = subprocess.check_output([helm, "template", "alpha", str(chart), "--values", str(values)], text=True)
    if rendered.count("kind: Deployment\n") != 2 or "kind: Job\n" not in rendered or f'ghcr.io/yanpgwang/mango:{manifest["version"]}' not in rendered:
        raise ValueError("packaged chart does not render the expected control-plane install")


def smoke(root, folder, helm="helm"):
    folder = folder.resolve()
    manifest = read_candidate(folder)
    with tempfile.TemporaryDirectory(prefix="mango-installed-release-") as temporary:
        destination = Path(temporary)
        inspect_commands(folder, manifest, destination)
        inspect_chart(folder, manifest, destination, helm)
        server = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        Fixture.failures, Fixture.calls = [], 0
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            url = f"http://127.0.0.1:{server.server_port}"
            inspect_python(folder, manifest, destination, url)
            inspect_typescript(root, folder, manifest, destination, url)
            inspect_go(folder, manifest, destination, url)
            if Fixture.failures or Fixture.calls != 6:
                raise ValueError(f"installed SDK request checks failed: {Fixture.failures}, {Fixture.calls} calls")
        finally:
            server.shutdown()
            server.server_close()
            thread.join()
    print(f"PASS: {manifest['version']} commands/chart, checksums, wheel/sdist/npm/Go-source fresh installs and six fixture requests")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("candidate", type=Path)
    parser.add_argument("--helm", default="helm")
    arguments = parser.parse_args()
    smoke(Path(__file__).resolve().parents[2], arguments.candidate, arguments.helm)
