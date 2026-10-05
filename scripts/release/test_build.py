"""Distribution safety tests; expectations do not use the builder's helpers."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from build import build, pack_binaries, pack_go_sdk, publish, snapshot_source, validate_source, write_manifest


VERSION = "0.1.0-alpha.2"
REVISION = "0123456789abcdef0123456789abcdef01234567"


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.folder = Path(self.temp.name)
        self.root = self.folder / "source"
        self.root.mkdir()
        files = {
            "sdk/typescript/package.json": '{"name":"mango-sdk","version":"0.1.0-alpha.2"}',
            "sdk/python/pyproject.toml": '[project]\nname="mango-sdk"\nversion="0.1.0a2"\n',
            "sdk/python/src/mango_sdk/_version.py": '__version__ = "0.1.0a2"\n',
            "sdk/go/version.go": 'package mango\nconst Version = "0.1.0-alpha.2"\n',
            "sdk/go/go.mod": 'module github.com/yanpgwang/mango/sdk/go\n\ngo 1.24.0\n',
            "sdk/go/LICENSE": "test license\n",
            "LICENSE": "test license\n",
        }
        for name, content in files.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
        self.git("init", "-q")
        self.commit()
        self.output = self.folder / "candidate"

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.root), *args], text=True).strip()

    def commit(self):
        self.git("add", ".")
        self.git("-c", "user.name=Distribution Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")

    def test_invalid_version_and_revision_do_not_create_output(self):
        for version in ["dev", "v0.1.0-alpha.2", "0.1.0", "../candidate", "0.1.0-alpha.02"]:
            with self.subTest(version=version), self.assertRaisesRegex(ValueError, "version"):
                validate_source(self.root, version, self.output)
        with self.assertRaisesRegex(ValueError, "revision"):
            validate_source(self.root, VERSION, self.output, REVISION)
        self.assertFalse(self.output.exists())

    def test_source_versions_must_match_across_all_sdks(self):
        path = self.root / "sdk/python/src/mango_sdk/_version.py"
        path.write_text('__version__ = "0.1.0a1"\n')
        self.commit()
        with self.assertRaisesRegex(ValueError, "Python"):
            validate_source(self.root, VERSION, self.output)
        self.assertFalse(self.output.exists())

    def test_dirty_tracked_or_untracked_source_is_rejected(self):
        path = self.root / "LICENSE"
        path.write_text("changed license\n")
        with self.assertRaisesRegex(ValueError, "clean"):
            validate_source(self.root, VERSION, self.output)
        self.git("restore", "LICENSE")
        (self.root / "untracked.go").write_text("package example\n")
        with self.assertRaisesRegex(ValueError, "clean"):
            validate_source(self.root, VERSION, self.output)

    def test_snapshot_uses_tracked_commit_bytes_only(self):
        (self.root / ".gitignore").write_text("ignored.go\n")
        self.commit()
        (self.root / "ignored.go").write_text("unreviewed build input")
        (self.root / "LICENSE").write_text("concurrent worktree edit")
        target = self.folder / "snapshot"
        snapshot_source(self.root, target, self.git("rev-parse", "HEAD"))
        self.assertEqual((target / "LICENSE").read_text(), "test license\n")
        self.assertFalse((target / "ignored.go").exists())

    def test_existing_destination_is_untouched(self):
        self.output.mkdir()
        marker = self.output / "operator.txt"
        marker.write_text("keep me")
        with self.assertRaisesRegex(ValueError, "exists"):
            validate_source(self.root, VERSION, self.output)
        self.assertEqual(marker.read_text(), "keep me")

    def test_publish_never_replaces_even_an_empty_directory(self):
        stage = self.folder / "stage"
        stage.mkdir()
        (stage / "manifest.json").write_text("candidate")
        self.output.mkdir()
        with self.assertRaises(FileExistsError):
            publish(stage, self.output)
        self.assertEqual(list(self.output.iterdir()), [])
        self.assertTrue((stage / "manifest.json").exists())
        self.output.rmdir()
        publish(stage, self.output)
        self.assertFalse(stage.exists())
        self.assertEqual((self.output / "manifest.json").read_text(), "candidate")

    def test_binary_archive_uses_only_explicit_distribution_inputs(self):
        for name in ["mango", "mango-worker", ".env"]:
            (self.folder / name).write_text(name)
        archive = self.folder / "binaries.tar.gz"
        pack_binaries(archive, {name: self.folder / name for name in ["mango", "mango-worker"]}, self.root / "LICENSE", "mango-0.1.0-alpha.2-linux-arm64")
        with tarfile.open(archive) as package:
            self.assertEqual(package.getnames(), ["mango-0.1.0-alpha.2-linux-arm64/mango", "mango-0.1.0-alpha.2-linux-arm64/mango-worker", "mango-0.1.0-alpha.2-linux-arm64/LICENSE"])
            self.assertEqual(package.getmembers()[0].mode, 0o755)

    def test_go_source_archive_excludes_credentials_and_build_inputs(self):
        (self.root / "sdk/go/.env").write_text("synthetic private marker")
        (self.root / "sdk/go/generate.py").write_text("build helper")
        self.commit()
        archive = self.folder / "go.zip"
        pack_go_sdk(self.root, archive, VERSION)
        with zipfile.ZipFile(archive) as package:
            self.assertEqual(set(package.namelist()), {"mango-sdk-go-0.1.0-alpha.2/version.go", "mango-sdk-go-0.1.0-alpha.2/go.mod", "mango-sdk-go-0.1.0-alpha.2/LICENSE"})

    def test_manifest_records_real_payloads_and_checksums(self):
        self.output.mkdir()
        (self.output / "client.zip").write_bytes(b"abc")
        versions = {"go": VERSION, "typescript": VERSION, "python": "0.1.0a2"}
        write_manifest(self.output, VERSION, REVISION, versions, [{"name": "client.zip", "kind": "go-sdk"}])
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual(manifest["revision"], REVISION)
        self.assertEqual(manifest["sdk_versions"], versions)
        self.assertEqual(manifest["artifacts"], [{"name": "client.zip", "kind": "go-sdk", "size_bytes": 3, "sha256": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}])
        for line in (self.output / "SHA256SUMS").read_text().splitlines():
            digest, name = line.split("  ")
            self.assertEqual(hashlib.sha256((self.output / name).read_bytes()).hexdigest(), digest)

    def test_missing_artifact_cannot_produce_complete_manifest(self):
        self.output.mkdir()
        with self.assertRaises(FileNotFoundError):
            write_manifest(self.output, VERSION, REVISION, {}, [{"name": "absent.zip", "kind": "go-sdk"}])
        self.assertFalse((self.output / "manifest.json").exists())

    def test_failed_compiler_leaves_no_candidate_or_staging_directory(self):
        tools = self.folder / "tools"
        tools.mkdir()
        compiler = tools / "go"
        compiler.write_text("#!/bin/sh\nexit 73\n")
        compiler.chmod(0o755)
        with patch.dict(os.environ, {"PATH": str(tools) + os.pathsep + os.environ["PATH"]}):
            with self.assertRaises(subprocess.CalledProcessError):
                build(self.root, self.output, VERSION, ["linux/arm64"])
        self.assertFalse(self.output.exists())
        self.assertEqual(list(self.folder.glob(".candidate-*")), [])


if __name__ == "__main__":
    unittest.main()
