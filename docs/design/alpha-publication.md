---
title: Alpha publication
description: Promote accepted artifacts without rebuilding their source.
---

# Alpha publication

A developer installing Mango needs matching runtime images, chart, commands and
first-party SDKs that can be retrieved publicly. A successful candidate build or
a draft release does not establish that availability.

Acceptance is one explicit, manually dispatched promotion of a successful
main-branch candidate. Validate the workflow, source revision, required checks,
complete artifact inventory, checksums and OCI identity before publication.
Publish the inspected bytes without rebuilding. Existing version tags, image
digests, registry files or release assets must agree exactly; conflicting
versions stop the run. A retry may reuse identical publication results.

Use GitHub's short-lived job token for images, tags and the release, and native
npm/PyPI Trusted Publishers for SDK uploads. Development and ordinary CI need no
publishing credentials. Only a main-branch publication job receives write/OIDC
permissions. Its `release` environment can carry maintainer-controlled policy.
The workflow defaults to validation only. Registry trust configuration is an
account prerequisite, documented alongside the exact workflow identity.

Before making the GitHub prerelease public, verify anonymous retrieval of both
image index digests, exact npm/PyPI archive bytes and the Go module tag. Record
source, candidate and promotion workflow evidence and explicit supported scope
in release notes. This is artifact promotion: checksums and workflow records do
not claim a signed build provenance attestation. In particular, disable npm's
automatic build provenance in a later promotion job rather than describe that
job's revision as the candidate's build source.

The supported developer preview covers installation, Mango process replacement
and Environment Work recovery. External Temporal database cold restore remains
experimental after an unexplained restored-server exit in CI; retaining its test
does not make it a release promise.

Non-goals are automatic version selection or release cadence, a generic registry
framework, changes to Mango's API, hosted sandbox integration, production HA,
upgrades, and lossless large-content transport. The bounded-runtime change uses
a new alpha version; the earlier alpha.2 candidate and draft keep their original
source and bytes.

## Maintainer setup and invocation

Configure a Trusted Publisher on each existing `mango-sdk` project at npm and
PyPI, with GitHub owner `yanpgwang`, repository `mango`, workflow filename
`release-publish.yml` and environment `release`. npm must permit direct
publishing for this workflow. No long-lived registry token belongs in GitHub
Secrets or Mango configuration. Account sign-in and two-factor checks remain
with the maintainer. GHCR packages must allow this repository's job token to
write and must be publicly readable. For first publication, the workflow first
uploads both indexes to digest destinations without creating version tags.
New GHCR packages start private: use each package's GitHub settings to make it
public, then retry the same candidate. Anonymous digest retrieval must pass
before writing image version tags or uploading native SDK distributions.
Authorization failures are never treated as evidence that a version is absent.

After the source's required main CI passes, dispatch **Release candidate** at
that source with the matching version. Record its successful run ID. Dispatch
**Publish developer preview** on `main` using that ID, exact version and full
revision, initially leaving `publish` false. The validation job performs no
registry writes. Once validation succeeds and account trust is configured,
dispatch the same input with `publish` true. The job transfers and publicly
verifies both accepted OCI indexes, uploads missing native SDK distributions,
creates exact source tags and verifies public retrieval before publishing the
GitHub prerelease.

Publication spans independent services and is not atomic. An interrupted run
may leave tags or some packages published while the release remains incomplete.
Rerun the same candidate; matching bytes are reused, conflicting bytes stop the
run. Never overwrite, retag, or rebuild a version to hide a partial failure.
The workflow uses npm's `alpha` tag for a new publish; installation instructions
pin the accepted version, so rerunning an older version does not need to move a
shared registry alias backwards. A failed publication is not reported as an
available matched distribution.
