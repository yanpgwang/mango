---
title: Release version automation
description: One matched version PR followed by explicit candidate acceptance and publication.
---

# Release version automation

A maintainer needs one reviewable runtime, chart and SDK version change with a
changelog. The version change must not publish packages or advance again while
that candidate awaits acceptance. Mango uses the official release-please action
for version selection, changelog generation and one root manifest PR.

`VERSION` is the current source version. The release-please manifest records the
same version for its root package, initially bootstrapped from the public alpha 3
prerelease. The native `simple` strategy and explicit `extra-files` update the Go
SDK, TypeScript package and both root lock versions, Helm version/appVersion and
marked current-version documentation. Make and the actual Helm/Kubernetes
acceptance readers consume `VERSION`; independent synthetic fixtures and
historical release records keep their own identities. Candidate building rejects
a requested version that disagrees with `VERSION` or the distributed metadata.

The native prerelease strategy, `alpha` type and both pre-major bump flags keep
fixes, features and development API changes on `0.1.0-alpha.N`. A deliberate move
to another version line requires a maintainer change to this policy. There is
no persistent `release-as` override.

## Python mapping

The official action v5.0.0 bundles release-please 17.6.0. Its Python updater writes
the SemVer string directly; Mango requires canonical PEP 440 `0.1.0aN` in both
source and distribution metadata. A small post-step therefore updates only the
three Python metadata/lock slots and explicit current-version documentation.
It verifies the native PR's source repository, author, branch, labels, file
allowlist, next alpha identity and native Go/TypeScript/chart metadata first.
The script runs from the workflow's trusted main checkout, reads PR files as
data and uses GitHub's `expectedHeadOid` compare-and-swap for one atomic commit.
A concurrent edit rejects the write. Retrying already canonical content makes
no further commit. No dependency versions or historical release files are rewritten.

## Review, acceptance and completion

The workflow runs on main pushes or manual dispatch, using only the ephemeral
`GITHUB_TOKEN`. The repository must allow Actions to create pull requests; its
default workflow token permission can remain read-only. Only this bot job has
the contents/pull-request/label write permissions needed to manage its PR.

GitHub's current workflow-trigger policy creates approval-required CI runs for
`GITHUB_TOKEN` pull-request opened, synchronize and reopened events. After the
bot workflow completes its Python mapping, a maintainer selects **Approve
workflows to run** for the final PR head and reviews its required checks.
Updates can require approval again. Verify the checks belong to that final
head before merging; do not infer passing CI from a successful bot run. If the
repository does not expose the documented approval runs, resolve that concrete
state before merging. No PAT, alternate CI dispatch or automatic merge is used.

The action and manifest both set `skip-github-release`. The bot never creates
release tags or publishes a GitHub release. Merging the version PR leaves the
native `autorelease: pending` label in place; upstream then refuses to open
another version PR. Tags from a partial publication do not complete that state.

Continue with the existing [candidate acceptance](../guides/release-candidates.md)
and [explicit publication](alpha-publication.md) workflows. On a later main push
or manual bot run, a narrow reconciliation step changes pending to tagged only
when the same version has a public prerelease with an exact source revision
containing the merged version PR. A draft, absent release, denied API request or
conflicting source cannot release the pending barrier. Label writes are ordered
so an interrupted reconciliation keeps pending until retry. The public release
record remains the authority for matched artifact availability.

The native configuration/updaters are tested with the exact bundled library in
`scripts/release/please-tests`, separately from Mango's runtime and SDK packages.
Tests cover fixes/features/breaking commits, all native updates and Python slots,
unchanged dependency locks, real candidate metadata validation and the upstream
merged-pending barrier even when a partial tag exists. Offline Python tests cover
the GitHub boundaries, retry/conflict behavior and public-release reconciliation.
