"""Independent rejection and retry contracts for artifact promotion."""

import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import promote
from build import write_manifest

VERSION = "0.1.0-alpha.3"
REVISION = "c" * 40


class PromotionTests(unittest.TestCase):
    def run_record(self):
        return {"path": ".github/workflows/release-build.yml", "event": "workflow_dispatch",
                "head_branch": "main", "head_sha": REVISION, "status": "completed",
                "conclusion": "success", "repository": {"full_name": "yanpgwang/mango"}}

    def test_rejects_wrong_run_identity_before_git_or_download(self):
        for field, value in (("path", ".github/workflows/ci.yml"), ("event", "pull_request"),
                             ("head_branch", "feature"), ("head_sha", "d" * 40),
                             ("status", "in_progress"), ("conclusion", "failure"),
                             ("repository", {"full_name": "someone/fork"})):
            with self.subTest(field=field):
                run = self.run_record()
                run[field] = value
                with patch.object(promote, "github", return_value=run), patch.object(promote.subprocess, "run") as execute:
                    with self.assertRaises(ValueError):
                        promote.accepted_run(123, VERSION, REVISION)
                    execute.assert_not_called()

    def test_failed_latest_main_run_cannot_reuse_earlier_green(self):
        responses = [self.run_record(), {"workflow_runs": [
            {"status": "completed", "conclusion": "failure"},
            {"status": "completed", "conclusion": "success"}]}]
        with patch.object(promote, "github", side_effect=responses), patch.object(promote.subprocess, "run"):
            with self.assertRaisesRegex(ValueError, "current main CI"):
                promote.accepted_run(123, VERSION, REVISION)

    def test_non_main_ancestor_is_rejected(self):
        with patch.object(promote, "github", return_value=self.run_record()), patch.object(
                promote.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "merge-base")):
            with self.assertRaises(subprocess.CalledProcessError):
                promote.accepted_run(123, VERSION, REVISION)

    def candidate(self, folder):
        # Explicit wire inventory, independent of the production allowlist.
        entries = [(f"mango-{VERSION}-{p}.tar.gz", "commands", p.replace("-", "/"))
                   for p in ("linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64")]
        entries.extend([
            (f"mango-sdk-go-{VERSION}.zip", "go-sdk", None),
            ("mango_sdk-0.1.0a3-py3-none-any.whl", "python-sdk", None),
            ("mango_sdk-0.1.0a3.tar.gz", "python-sdk", None),
            (f"mango-sdk-{VERSION}.tgz", "typescript-sdk", None),
            (f"mango-{VERSION}.tgz", "helm-chart", None),
            ("mango-oci.tar", "oci-image", None),
            ("mango-self-hosted-worker-oci.tar", "oci-image", None),
        ])
        records = []
        for name, kind, platform in entries:
            (folder / name).write_bytes(b"independent fixture bytes: " + name.encode())
            record = {"name": name, "kind": kind}
            if platform:
                record["platform"] = platform
            if kind == "oci-image":
                record.update(image=name.removesuffix("-oci.tar"), digest="sha256:" + "a" * 64,
                              platforms=["linux/amd64", "linux/arm64"])
            records.append(record)
        write_manifest(folder, VERSION, REVISION,
                       {"go": VERSION, "typescript": VERSION, "python": "0.1.0a3"}, records)

    def test_rejects_unlisted_payload_even_with_valid_checksums(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.candidate(folder)
            (folder / "extra.bin").write_bytes(b"not part of accepted distribution")
            with self.assertRaisesRegex(ValueError, "unexpected file"):
                promote.validate(folder, VERSION, REVISION)

    def test_metadata_identity_does_not_substitute_for_requested_source(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.candidate(folder)
            with self.assertRaisesRegex(ValueError, "identity differs"):
                promote.validate(folder, VERSION, "d" * 40)

    def test_image_metadata_is_independently_reinspected(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.candidate(folder)
            with patch.object(promote, "inspect_oci", return_value={
                    "digest": "sha256:" + "b" * 64, "platforms": ["linux/amd64", "linux/arm64"]}):
                with self.assertRaisesRegex(ValueError, "inspected archive"):
                    promote.validate(folder, VERSION, REVISION)

    def test_modified_archive_is_rejected_before_oci_inspection(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            self.candidate(folder)
            (folder / f"mango-sdk-{VERSION}.tgz").write_bytes(b"changed")
            with patch.object(promote, "inspect_oci") as inspect:
                with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                    promote.validate(folder, VERSION, REVISION)
                inspect.assert_not_called()

    def test_matching_tag_is_reused_without_mutation_but_conflict_is_rejected(self):
        for target in (REVISION, "d" * 40):
            with self.subTest(target=target), patch.object(promote.subprocess, "run", return_value=
                    subprocess.CompletedProcess([], 0, json.dumps({"object": {"type": "commit", "sha": target}}), "")), patch.object(promote, "github") as mutate:
                if target == REVISION:
                    promote.ensure_tag("v" + VERSION, REVISION)
                else:
                    with self.assertRaisesRegex(ValueError, "tag conflicts"):
                        promote.ensure_tag("v" + VERSION, REVISION)
                mutate.assert_not_called()

    def test_tag_authentication_error_never_becomes_absence(self):
        with patch.object(promote.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "HTTP 403")), patch.object(promote, "github") as mutate:
            with self.assertRaisesRegex(ValueError, "cannot inspect"):
                promote.ensure_tag("v" + VERSION, REVISION)
            mutate.assert_not_called()

    def test_missing_tag_preflight_never_creates_it(self):
        with patch.object(promote.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "HTTP 404")), patch.object(promote, "github") as mutate:
            promote.ensure_tag("v" + VERSION, REVISION, create=False)
            mutate.assert_not_called()

    def test_private_first_packages_bootstrap_by_digest_without_writing_version_tags(self):
        records = [{"name": name + "-oci.tar", "kind": "oci-image", "image": name,
                    "digest": "sha256:" + digest * 64}
                   for name, digest in (("mango", "a"), ("mango-self-hosted-worker", "b"))]
        args = SimpleNamespace(folder=Path("/accepted"), version=VERSION, revision=REVISION)
        with patch.object(promote, "validate", return_value={"artifacts": records}), patch.object(
                promote.subprocess, "run") as copy, patch.object(
                promote, "verify_public_image", side_effect=ValueError("package starts private")):
            with self.assertRaisesRegex(ValueError, "starts private"):
                promote.images(args)
            self.assertEqual(2, copy.call_count)
            for call, record in zip(copy.call_args_list, records):
                self.assertEqual("docker://ghcr.io/yanpgwang/" + record["image"] + "@" + record["digest"], call.args[0][-1])
                self.assertIn("--preserve-digests", call.args[0])

    def test_an_existing_conflicting_image_version_is_never_retagged(self):
        record = {"name": "mango-oci.tar", "kind": "oci-image", "image": "mango", "digest": "sha256:" + "a" * 64}
        args = SimpleNamespace(folder=Path("/accepted"), version=VERSION, revision=REVISION)
        def execute(arguments, **kwargs):
            if "inspect" in arguments:
                return subprocess.CompletedProcess(arguments, 0, b"different accepted index", b"")
            return subprocess.CompletedProcess(arguments, 0)
        with patch.object(promote, "validate", return_value={"artifacts": [record]}), patch.object(
                promote.subprocess, "run", side_effect=execute) as run, patch.object(promote, "verify_public_image"):
            with self.assertRaisesRegex(ValueError, "image tag conflicts"):
                promote.images(args)
            copies = [c.args[0] for c in run.call_args_list if "copy" in c.args[0]]
            self.assertEqual(1, len(copies))
            self.assertIn("@sha256:", copies[0][-1])

    def test_retry_reuses_only_identical_public_sdk_bytes(self):
        data = b"accepted npm bytes"
        record = {"name": "accepted.tgz", "kind": "typescript-sdk", "size_bytes": len(data),
                  "sha256": promote.hashlib.sha256(data).hexdigest()}
        manifest = {"version": VERSION, "sdk_versions": {"python": "0.1.0a3"}, "artifacts": [record]}
        npm = json.dumps({"name": "mango-sdk", "version": VERSION,
                          "dist": {"tarball": "https://registry.npmjs.org/mango-sdk/-/accepted.tgz"}}).encode()
        with patch.object(promote, "fetch", side_effect=[npm, data, None]):
            self.assertEqual((False, []), promote.registry_state(manifest))
        with patch.object(promote, "fetch", side_effect=[npm, b"conflict"]):
            with self.assertRaisesRegex(ValueError, "registry bytes conflict"):
                promote.registry_state(manifest)

    def test_registry_artifact_host_is_restricted(self):
        with patch.object(promote, "fetch") as fetch:
            with self.assertRaisesRegex(ValueError, "artifact host"):
                promote.verify_download("https://example.com/fake.tgz", {}, "registry.npmjs.org")
            fetch.assert_not_called()


if __name__ == "__main__":
    unittest.main()
