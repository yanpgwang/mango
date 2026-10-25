"""Narrow release-please handoffs: canonical Python slots and public completion."""

import argparse
import base64
import json
from pathlib import Path
import re
import subprocess
import urllib.parse

from build import validate_chart_metadata
from promote import REPOSITORY, github


BRANCH = "release-please--branches--main"
PENDING = "autorelease: pending"
TAGGED = "autorelease: tagged"
ALPHA = re.compile(r"0\.1\.0-alpha\.(0|[1-9][0-9]*)")
PYTHON_FILES = (
    "sdk/python/pyproject.toml", "sdk/python/src/mango_sdk/_version.py", "sdk/python/uv.lock",
    "sdk/python/README.md", "sdk/README.md", "sdk/RELEASING.md", "docs/sdk.md",
    "docs/sdk/python.md", "docs/design/kubernetes-alpha.md", "docs/guides/release-candidates.md",
)


def python_version(version):
    match = ALPHA.fullmatch(version)
    if not match:
        raise ValueError("release-please must target canonical 0.1.0-alpha.N")
    return "0.1.0a" + match[1]


def python_updates(contents, current, target):
    """Change only known version slots; never rewrite Python dependencies."""
    old, new = python_version(current), python_version(target)
    updates = {}
    for path in PYTHON_FILES:
        content = contents[path]
        if path.endswith("pyproject.toml"):
            pattern = r'(?m)^(version = ")([^"\n]+)(")$'
        elif path.endswith("_version.py"):
            pattern = r'(?m)^(__version__ = ")([^"\n]+)(")$'
        elif path.endswith("uv.lock"):
            pattern = r'(?m)(\[\[package\]\]\nname = "mango-sdk"\nversion = ")([^"\n]+)(")'
        else:
            # These explicit current-version documents contain no historical
            # Python releases. Native generic updaters handle their SemVer slots.
            old_slot = re.compile(rf"(?<![A-Za-z0-9]){re.escape(old)}(?![A-Za-z0-9])")
            new_slot = re.compile(rf"(?<![A-Za-z0-9]){re.escape(new)}(?![A-Za-z0-9])")
            if not old_slot.search(content) and not new_slot.search(content):
                raise ValueError(f"missing current Python version slot: {path}")
            changed = old_slot.sub(new, content)
            if changed != content:
                updates[path] = changed
            continue
        matches = list(re.finditer(pattern, content))
        if len(matches) != 1 or matches[0][2] not in {old, new}:
            raise ValueError(f"unexpected Python version slot: {path}")
        changed = re.sub(pattern, lambda match: match[1] + new + match[3], content)
        if changed != content:
            updates[path] = changed
    return updates


def eligible(pr, merged):
    return (pr.get("merged") is merged and pr.get("base", {}).get("ref") == "main"
            and pr.get("base", {}).get("repo", {}).get("full_name") == REPOSITORY
            and pr.get("head", {}).get("repo", {}).get("full_name") == REPOSITORY
            and pr.get("head", {}).get("ref") == BRANCH
            and pr.get("user", {}).get("login") == "github-actions[bot]"
            and PENDING in {label["name"] for label in pr.get("labels", [])})


def file_at(path, revision):
    item = github(f"contents/{path}?ref={revision}")
    if item.get("type") != "file" or item.get("encoding") != "base64":
        raise ValueError(f"expected an ordinary release file: {path}")
    return base64.b64decode(item["content"]).decode()


def commit_python(branch, expected_head, updates):
    if (branch != BRANCH or not re.fullmatch(r"[0-9a-f]{40}", expected_head)
            or not updates or not set(updates).issubset(PYTHON_FILES)):
        raise ValueError("refusing a commit outside the Python release slots")
    # expectedHeadOid is an atomic compare-and-swap. A concurrent edit rejects
    # this entire commit; no force-push or PR code execution is involved.
    mutation = """mutation($input: CreateCommitOnBranchInput!) {
      createCommitOnBranch(input: $input) { commit { oid } }
    }"""
    body = {"query": mutation, "variables": {"input": {
        "branch": {"repositoryNameWithOwner": REPOSITORY, "branchName": branch},
        "expectedHeadOid": expected_head,
        "message": {"headline": "chore: canonical Python alpha version"},
        "fileChanges": {"additions": [
            {"path": path, "contents": base64.b64encode(content.encode()).decode()}
            for path, content in sorted(updates.items())
        ]},
    }}}
    result = json.loads(subprocess.check_output(
        ["gh", "api", "graphql", "--input", "-"], input=json.dumps(body), text=True))
    if result.get("errors"):
        raise ValueError("Python version commit rejected; retry against the current bot head")
    return result["data"]["createCommitOnBranch"]["commit"]["oid"]


def canonicalize(root):
    current = (root / "VERSION").read_text().strip()
    python_version(current)
    query = urllib.parse.urlencode({"state": "open", "base": "main", "head": f"yanpgwang:{BRANCH}"})
    prs = github("pulls?" + query)
    if not prs:
        print("No open release-please PR needs Python mapping")
        return
    if len(prs) != 1:
        raise ValueError("expected one root release-please PR")
    pr = github(f"pulls/{prs[0]['number']}")
    if not eligible(pr, False) or pr.get("state") != "open":
        raise ValueError("refusing to modify an unrelated pull request")
    head = pr["head"]["sha"]
    if not re.fullmatch(r"[0-9a-f]{40}", head):
        raise ValueError("expected full bot PR head")
    config = json.loads((root / "release-please-config.json").read_text())
    allowed = {item if isinstance(item, str) else item["path"]
               for item in config["packages"]["."]["extra-files"]}
    allowed |= set(PYTHON_FILES) | {"VERSION", "CHANGELOG.md", ".release-please-manifest.json"}
    changed = github(f"pulls/{pr['number']}/files?per_page=100")
    if (len(changed) != pr["changed_files"] or any(
            item["filename"] not in allowed or item["status"] not in {"modified", "added"}
            or (item["status"] == "added" and item["filename"] != "CHANGELOG.md") for item in changed)):
        raise ValueError("bot PR contains changes outside the release file allowlist")
    version = file_at("VERSION", head).strip()
    python_version(version)
    if int(ALPHA.fullmatch(version)[1]) != int(ALPHA.fullmatch(current)[1]) + 1:
        raise ValueError("bot PR must advance the current alpha by exactly one")
    manifest = json.loads(file_at(".release-please-manifest.json", head))
    if manifest != {".": version} or pr["title"] != f"chore(main): release {version}":
        raise ValueError("bot PR identity differs from VERSION")
    # Check the native updater's metadata before adding canonical Python slots.
    if json.loads(file_at("sdk/typescript/package.json", head))["version"] != version:
        raise ValueError("TypeScript version differs from VERSION")
    lock = json.loads(file_at("sdk/typescript/package-lock.json", head))
    if lock["version"] != version or lock["packages"][""]["version"] != version:
        raise ValueError("TypeScript lock versions differ from VERSION")
    go = file_at("sdk/go/version.go", head)
    if not re.search(rf'^const Version = "{re.escape(version)}"$', go, re.MULTILINE):
        raise ValueError("Go SDK version differs from VERSION")
    validate_chart_metadata(file_at("charts/mango/Chart.yaml", head), version)
    contents = {path: file_at(path, head) for path in PYTHON_FILES}
    updates = python_updates(contents, current, version)
    if updates:
        committed = commit_python(BRANCH, head, updates)
        print(f"Canonical Python version committed at {committed}; approve CI for that final PR head")
    else:
        print(f"Canonical Python version already present at {head}")


def public_release(version):
    result = subprocess.run(["gh", "api", f"repos/{REPOSITORY}/releases/tags/v{version}"],
                            capture_output=True, text=True)
    if result.returncode:
        if "HTTP 404" in result.stderr:
            return None
        raise ValueError("cannot inspect public release completion")
    return json.loads(result.stdout)


def reconcile():
    # A partial publication may already have tags. Only a public prerelease
    # from the existing verified publisher completes this coordination state.
    page = 1
    while True:
        query = urllib.parse.urlencode({"state": "closed", "labels": PENDING, "per_page": 100, "page": page})
        issues = github("issues?" + query)
        for issue in issues:
            if "pull_request" not in issue:
                continue
            pr = github(f"pulls/{issue['number']}")
            if not eligible(pr, True):
                continue
            match = re.fullmatch(r"chore\(main\): release (0\.1\.0-alpha\.(?:0|[1-9][0-9]*))", pr["title"])
            if not match:
                raise ValueError("merged pending release PR has an unexpected version title")
            version = match[1]
            release = public_release(version)
            if release is None or release.get("draft") is True:
                print(f"Release {version} remains pending public publication")
                continue
            source = release.get("target_commitish", "")
            if (release.get("tag_name") != "v" + version or release.get("draft") is not False
                    or release.get("prerelease") is not True
                    or not re.fullmatch(r"[0-9a-f]{40}", source)):
                raise ValueError("public prerelease identity conflicts with the pending version PR")
            comparison = github(f"compare/{pr['merge_commit_sha']}...{source}")
            if comparison["status"] not in {"ahead", "identical"}:
                raise ValueError("public release source does not contain the merged version PR")
            # Add first: a crash must leave pending in place until retry.
            github(f"issues/{pr['number']}/labels", "POST", {"labels": [TAGGED]})
            github(f"issues/{pr['number']}/labels/{urllib.parse.quote(PENDING, safe='')}", "DELETE")
            print(f"Public prerelease {version} completes version PR #{pr['number']}")
        if len(issues) < 100:
            break
        page += 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("reconcile", "canonicalize"))
    args = parser.parse_args()
    if args.operation == "reconcile":
        reconcile()
    else:
        canonicalize(Path(__file__).resolve().parents[2])


if __name__ == "__main__":
    main()
