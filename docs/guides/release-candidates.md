---
title: Release candidates
description: Build and inspect matched Mango runtime and SDK artifacts before publication.
---

# Release candidates

A candidate contains runtime commands and native SDK packages from one clean
Git revision. Candidate tooling does not publish a runtime release, change npm
or PyPI packages, or establish Kubernetes support. The
[Kubernetes alpha design](../design/kubernetes-alpha.md) records the remaining
chart, cluster/recovery and publication acceptance.

## Build and inspect

Use Go 1.26+, Node.js 22+, uv, Git and a Linux or macOS builder. The Make targets
select Python 3.12 through uv. Start from a clean checkout whose SDK versions
match the requested alpha; local changes and existing output directories are
rejected. Source is copied from the tracked commit into an isolated build tree,
so ignored local files cannot affect compilation or packaging.

```sh
make release-unit
make release-build RELEASE_VERSION=0.1.0-alpha.2
make release-check RELEASE_VERSION=0.1.0-alpha.2
```

The default output is `dist/release/0.1.0-alpha.2`. `manifest.json` records the
full source revision, language versions, platform and SHA-256 for each archive.
`SHA256SUMS` also covers the manifest. Check that index before installing:

```sh
cd dist/release/0.1.0-alpha.2
sha256sum -c SHA256SUMS
```

On macOS, use `shasum -a 256 -c SHA256SUMS`. Checksums detect changed bytes;
they do not authenticate a candidate obtained from an untrusted publisher.

Command archives cover Linux and macOS, AMD64 and ARM64. Each contains `mango`,
`mango-worker` and the license. Both commands report the same injected identity:

```sh
./mango version
./mango-worker version
```

No database, model or Docker connection is needed for those commands. The
reference Docker supervisor remains operator-run; its item image is selected
explicitly with `--image` or `MANGO_WORKER_IMAGE` when using a release image.

`release-check` verifies archive contents and linked identity, creates fresh
SDK installations outside the checkout, checks installed TypeScript
declarations, and exercises six authenticated requests against an explicitly
simulated HTTP fixture. The owning raw Mango HTTP/SDK conformance and real
service tests remain separate checks. No proposed sandbox tools or hosted agent
service execute during package inspection.

## Candidate SDK installation

TypeScript candidates include compiled JavaScript and declarations. Python
candidates include a wheel and source distribution, both exposing the same
version. Install the inspected files directly in your application's environment:

```sh
npm install /absolute/path/to/mango-sdk-0.1.0-alpha.2.tgz
python -m pip install /absolute/path/to/mango_sdk-0.1.0a2-py3-none-any.whl
```

The Go source zip is an independently buildable module. Extract it and use a
local `replace` from your application until the matching Go module tag is
published, as described in [SDK installation](../sdk.md#install-from-source).
Candidates do not change the public registry status: npm/PyPI alpha 1 remains
the older published interface; alpha 2 has not been published.

## Multi-platform OCI candidates

The manual GitHub Actions **Release candidate** workflow builds the command and
SDK archives, then exports the control-plane and reference-worker OCI images
for Linux AMD64 and ARM64. It requires only read access to source, performs no
registry login or push, and uploads one candidate artifact retained for 14 days.

OCI exports are named `mango-oci.tar` and
`mango-self-hosted-worker-oci.tar`. The image checker verifies metadata blob
digests, platform selection, numeric non-root identity and matching version/full
revision, then adds both archives to the manifest/checksum index. These checks
are specific to Mango's Buildx output, not a general OCI importer or validator.
Use OCI-aware tooling for inspection or registry transfer; do not assume that a
Docker installation with a classic image store can load a multi-platform OCI
archive directly.

Local exports may also be added with
`python scripts/release/images.py /absolute/path/to/candidate` after building
both OCI archives from that exact revision. A metadata mismatch fails before
the manifest changes. Candidate inspection is preparation for a later reviewed
release; it is not an automatic publishing path.
