"""Release coordinator boundaries, independent of live GitHub credentials."""

import base64
import json
from pathlib import Path
from subprocess import CompletedProcess
import unittest
from unittest.mock import patch

import please


ROOT = Path(__file__).resolve().parents[2]
HEAD = "1" * 40
SOURCE = "2" * 40


def pr(merged=False):
    return {"number": 42, "title": "chore(main): release 0.1.0-alpha.4", "merged": merged,
            "merge_commit_sha": HEAD, "state": "closed" if merged else "open",
            "user": {"login": "github-actions[bot]"}, "labels": [{"name": please.PENDING}],
            "base": {"ref": "main", "repo": {"full_name": please.REPOSITORY}},
            "head": {"ref": please.BRANCH, "sha": HEAD, "repo": {"full_name": please.REPOSITORY}}}


class PythonMappingTests(unittest.TestCase):
    def contents(self):
        return {path: (ROOT / path).read_text() for path in please.PYTHON_FILES}

    def versions(self):
        current = (ROOT / "VERSION").read_text().strip()
        return current, "0.1.0-alpha." + str(int(please.ALPHA.fullmatch(current)[1]) + 1)

    def test_only_canonical_version_slots_change_and_retry_is_a_noop(self):
        current, target = self.versions()
        before = self.contents()
        changes = please.python_updates(before, current, target)
        self.assertEqual(set(changes), set(please.PYTHON_FILES))
        # In particular, httpx/dev-dependency lock entries and their hashes survive.
        old, new = please.python_version(current), please.python_version(target)
        for path in please.PYTHON_FILES:
            self.assertEqual(changes[path].replace(new, old), before[path])
        self.assertEqual(please.python_updates({**before, **changes}, current, target), {})

    def test_unexpected_or_duplicate_metadata_slot_fails_instead_of_broad_replacement(self):
        current, target = self.versions()
        old = please.python_version(current)
        unexpected = "0.1.0a" + str(int(please.ALPHA.fullmatch(target)[1]) + 1)
        for path, addition in [("sdk/python/pyproject.toml", f'\nversion = "{old}"\n'),
                               ("sdk/python/src/mango_sdk/_version.py", f'\n__version__ = "{unexpected}"\n')]:
            contents = self.contents()
            contents[path] += addition
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, f"slot: {path}"):
                please.python_updates(contents, current, target)
        for version in ["0.1.0-alpha.04", "0.2.0-alpha.1", "0.1.0", "0.1.0a4"]:
            with self.subTest(version=version), self.assertRaises(ValueError):
                please.python_version(version)

    def test_document_mapping_does_not_change_a_longer_alpha_number(self):
        current, target = self.versions()
        longer = please.python_version(current) + "0"
        contents = self.contents()
        contents["docs/sdk/python.md"] += f"\nOther version {longer} remains distinct.\n"
        changes = please.python_updates(contents, current, target)
        self.assertIn(please.python_version(target), changes["docs/sdk/python.md"])
        self.assertIn(f"Other version {longer} remains distinct.", changes["docs/sdk/python.md"])

    def test_graphql_commit_requires_the_expected_head_and_contains_only_allowed_python_files(self):
        response = {"data": {"createCommitOnBranch": {"commit": {"oid": SOURCE}}}}
        with patch("please.subprocess.check_output", return_value=json.dumps(response)) as command:
            self.assertEqual(please.commit_python(please.BRANCH, HEAD, {"sdk/python/pyproject.toml": "test"}), SOURCE)
        body = json.loads(command.call_args.kwargs["input"])["variables"]["input"]
        self.assertEqual(body["expectedHeadOid"], HEAD)
        self.assertEqual(body["branch"], {"repositoryNameWithOwner": please.REPOSITORY, "branchName": please.BRANCH})
        self.assertEqual(body["fileChanges"]["additions"], [
            {"path": "sdk/python/pyproject.toml", "contents": base64.b64encode(b"test").decode()}])
        with patch("please.subprocess.check_output", return_value=json.dumps({"errors": [{"message": "head changed"}]})):
            with self.assertRaisesRegex(ValueError, "rejected"):
                please.commit_python(please.BRANCH, HEAD, {"sdk/python/pyproject.toml": "test"})

    def test_unrelated_bot_or_fork_pr_is_never_eligible(self):
        for field in ["user", "head", "base", "labels"]:
            item = pr()
            item[field] = {} if field != "labels" else []
            with self.subTest(field=field):
                self.assertFalse(please.eligible(item, False))


class CompletionTests(unittest.TestCase):
    def api(self, path, method="GET", body=None):
        if path.startswith("issues?"):
            return [{"number": 42, "pull_request": {}}]
        if path == "pulls/42":
            return pr(True)
        if path.startswith("compare/"):
            return {"status": "ahead"}
        if "/labels" in path:
            return []
        raise AssertionError(path)

    def test_tags_or_a_draft_do_not_complete_a_merged_version(self):
        for release in [None, {"draft": True}]:
            with self.subTest(release=release), patch("please.github", side_effect=self.api) as api, patch("please.public_release", return_value=release):
                please.reconcile()
                self.assertTrue(all(call.args[0].split("?")[0] in {"issues", "pulls/42"} for call in api.call_args_list))
                self.assertFalse(any("tags" in call.args[0] for call in api.call_args_list))

    def test_only_a_matching_public_prerelease_containing_the_merge_releases_the_mutex(self):
        release = {"tag_name": "v0.1.0-alpha.4", "draft": False, "prerelease": True, "target_commitish": SOURCE}
        with patch("please.github", side_effect=self.api) as api, patch("please.public_release", return_value=release):
            please.reconcile()
        calls = api.call_args_list
        self.assertIn(f"compare/{HEAD}...{SOURCE}", [call.args[0] for call in calls])
        writes = [call for call in calls if len(call.args) > 1]
        self.assertEqual(writes[0].args, ("issues/42/labels", "POST", {"labels": [please.TAGGED]}))
        self.assertEqual(writes[1].args, ("issues/42/labels/autorelease%3A%20pending", "DELETE"))

    def test_conflicting_or_non_descendant_public_release_cannot_clear_pending(self):
        release = {"tag_name": "v0.1.0-alpha.4", "draft": False, "prerelease": True, "target_commitish": SOURCE}
        for changed in [{**release, "tag_name": "v0.1.0-alpha.5"}, {**release, "prerelease": False},
                        {**release, "target_commitish": "main"}, {**release, "draft": None}, release]:
            def api(path, *args):
                if path.startswith("compare/"):
                    return {"status": "diverged"}
                return self.api(path, *args)
            with self.subTest(release=changed), patch("please.github", side_effect=api) as calls, patch("please.public_release", return_value=changed):
                with self.assertRaises(ValueError):
                    please.reconcile()
                self.assertFalse(any(len(call.args) > 1 for call in calls.call_args_list))

    def test_authorization_error_is_not_treated_as_absence(self):
        denied = CompletedProcess([], 1, stdout="", stderr="HTTP 403 denied")
        missing = CompletedProcess([], 1, stdout="", stderr="HTTP 404 missing")
        with patch("please.subprocess.run", return_value=denied), self.assertRaisesRegex(ValueError, "inspect"):
            please.public_release("0.1.0-alpha.4")
        with patch("please.subprocess.run", return_value=missing):
            self.assertIsNone(please.public_release("0.1.0-alpha.4"))


class CanonicalizeBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.current = (ROOT / "VERSION").read_text().strip()
        self.target = "0.1.0-alpha." + str(int(please.ALPHA.fullmatch(self.current)[1]) + 1)
        config = json.loads((ROOT / "release-please-config.json").read_text())
        paths = {item if isinstance(item, str) else item["path"]
                 for item in config["packages"]["."]["extra-files"]}
        paths |= set(please.PYTHON_FILES) | {"VERSION", ".release-please-manifest.json"}
        self.contents = {path: (ROOT / path).read_text().replace(self.current, self.target) for path in paths}
        self.item = pr()
        self.item["title"] = f"chore(main): release {self.target}"
        self.files = [{"filename": "VERSION", "status": "modified"}]
        self.item["changed_files"] = len(self.files)

    def api(self, path):
        if path.startswith("pulls?"):
            return [{"number": 42}]
        if path == "pulls/42":
            return self.item
        if path == "pulls/42/files?per_page=100":
            return self.files
        raise AssertionError(path)

    def file_at(self, path, head):
        self.assertEqual(head, HEAD)
        return self.contents[path]

    def test_verified_native_pr_is_mapped_once_and_retry_does_not_create_another_commit(self):
        with patch("please.github", side_effect=self.api), patch("please.file_at", side_effect=self.file_at), patch("please.commit_python", return_value=SOURCE) as commit:
            please.canonicalize(ROOT)
            branch, head, changes = commit.call_args.args
            self.assertEqual((branch, head), (please.BRANCH, HEAD))
            self.assertEqual(set(changes), set(please.PYTHON_FILES))
            self.contents.update(changes)
            commit.reset_mock()
            please.canonicalize(ROOT)
            commit.assert_not_called()

    def test_unsafe_file_scope_or_incomplete_pagination_is_rejected_before_reading_pr_content(self):
        for files, total in [([{"filename": "internal/runtime.go", "status": "modified"}], 1),
                             ([{"filename": "VERSION", "status": "renamed"}], 1), (self.files, 101)]:
            self.files, self.item["changed_files"] = files, total
            with self.subTest(files=files), patch("please.github", side_effect=self.api), patch("please.file_at") as read, patch("please.commit_python") as commit:
                with self.assertRaisesRegex(ValueError, "allowlist"):
                    please.canonicalize(ROOT)
                read.assert_not_called()
                commit.assert_not_called()

    def test_wrong_manifest_or_native_lock_cannot_be_reported_as_a_completed_mapping(self):
        for path, changed in [(".release-please-manifest.json", json.dumps({".": "0.1.0-alpha.99"})),
                              ("sdk/typescript/package-lock.json", json.dumps({"version": self.current, "packages": {"": {"version": self.target}}}))]:
            before = self.contents[path]
            self.contents[path] = changed
            with self.subTest(path=path), patch("please.github", side_effect=self.api), patch("please.file_at", side_effect=self.file_at), patch("please.commit_python") as commit:
                with self.assertRaises(ValueError):
                    please.canonicalize(ROOT)
                commit.assert_not_called()
            self.contents[path] = before

    def test_concurrent_head_rejection_fails_the_handoff(self):
        with patch("please.github", side_effect=self.api), patch("please.file_at", side_effect=self.file_at), patch("please.commit_python", side_effect=ValueError("expected head changed")):
            with self.assertRaisesRegex(ValueError, "head changed"):
                please.canonicalize(ROOT)


if __name__ == "__main__":
    unittest.main()
