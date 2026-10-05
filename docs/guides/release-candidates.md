---
title: Release candidates
description: Build and install matched runtime, chart and SDK artifacts before publication.
---

# Release candidates

A candidate contains runtime commands, a packaged Helm chart and native SDK
packages from one clean Git revision. The manual candidate workflow also exports
both OCI images and installs the actual artifacts in an isolated Kubernetes
lifecycle test. The [alpha design](../design/kubernetes-alpha.md) defines the
release scope. Building and validating candidates does not publish registries.

## Build and inspect

Use Go 1.26+, Node.js 22+, uv, Git, Helm 4.3.0 and a Linux or macOS builder. The Make targets
select Python 3.12 through uv. Start from a clean checkout whose SDK versions
match the requested alpha; local changes and existing output directories are
rejected. Choose an output directory outside the checkout or inside an ignored
parent directory such as `dist/release`; other in-checkout paths fail before building.
Source is copied from the tracked commit into an isolated build tree,
so ignored local files cannot affect compilation or packaging.

```sh
make release-unit
make release-build RELEASE_VERSION=0.1.0-alpha.2
make release-check RELEASE_VERSION=0.1.0-alpha.2
```

The default output is `dist/release/0.1.0-alpha.2`. `manifest.json` records the
full source revision, language versions, platform and SHA-256 for each archive.
Chart version and `appVersion` must match the runtime/SDK candidate. Helm packages
the tracked source without overriding that metadata; mismatches fail before builds.
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

`release-check` verifies archive contents and linked identity, checks and renders
the packaged chart with explicit operator inputs, creates fresh
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
SDK/chart archives, then exports the control-plane and reference-worker OCI images
for Linux AMD64 and ARM64. Both image builds also use the tracked Git snapshot,
so ignored local files are never compiler inputs. It requires only read access
to source, performs no registry login or push, and uploads one candidate artifact
retained for 14 days after artifact installation/recovery passes.

OCI exports are named `mango-oci.tar` and
`mango-self-hosted-worker-oci.tar`. The image checker verifies metadata blob
digests, platform selection, numeric non-root identity and matching version/full
revision, then adds both archives and their top-level image index digests to the
manifest/checksum index. These checks
are specific to Mango's Buildx output, not a general OCI importer or validator.
Use OCI-aware tooling for inspection or registry transfer; do not assume that a
Docker installation with a classic image store can load a multi-platform OCI
archive directly.

Local exports may also be added with
`python scripts/release/images.py /absolute/path/to/candidate` after building
both OCI archives from that exact revision. A metadata mismatch fails before
the manifest changes. Candidate inspection is preparation for a later reviewed
release; it is not an automatic publishing path.

## Install and recover the actual candidate

After OCI finalization, the common candidate has eleven payloads: four paired
command archives, four SDK packages, one chart and two OCI archives. The full
artifact acceptance tier requires the same source checkout as the manifest,
Docker 28+ with its containerd image store, kind 0.33.0, kubectl 1.37.0 and Helm
4.3.0. The containerd requirement is for importing Buildx OCI archives in this
test; normal published-image consumers pull their native image from a registry.

```sh
MANGO_RELEASE_CANDIDATE=/absolute/path/to/candidate \
  make test-kubernetes KIND=kind KUBECTL=kubectl HELM=helm
```

This verifies every listed payload's bytes and source identity before importing
images. It runs the native supervisor from the command archive, installs the
packaged chart and uses both inspected release images. Only the explicitly
simulated Messages fixture is built from source. The existing authenticated
HTTP, external sandbox, process replacement and original-Temporal-history
restore journey runs against those artifacts. Fixtures own their cluster,
state stores, aliases and volumes; configured provider credentials are unused.

The manual workflow runs this tier on Linux AMD64 with Docker 28.1.1;
local acceptance also exercises macOS ARM64 and the ARM64 images. The default
required **Kubernetes lifecycle** job retains the source-build contributor path.
Fresh SDK installation checks remain separate from runtime durability tests.

## Publication boundary

Publish only a candidate built from the reviewed merged revision after its
required CI and artifact acceptance pass. Keep the inspected archives unchanged,
use their recorded OCI index digests, and tag the Go module from that same source
with `sdk/go/v0.1.0-alpha.2`. Registry publication requires maintainer credentials;
those are never development or CI dependencies.

Release notes record the actual source, checksums, image digests, successful
candidate workflow and supported lifecycle. Operators install the inspected
`mango-0.1.0-alpha.2.tgz` with their existing Secrets and dependency values:

```sh
helm install mango ./mango-0.1.0-alpha.2.tgz --namespace mango --create-namespace \
  --values operator-values.yaml \
  --set image.repository=ghcr.io/yanpgwang/mango \
  --set 'image.digest=sha256:<digest-from-the-verified-release-record>'
```

Select the matching published reference-worker image for the external
`mango-worker docker --image` invocation. Follow the
[Kubernetes guide](kubernetes.md) for dependency preparation, Environment keys,
backup/key preservation and same-release restore. A candidate's existence does
not promise registry availability, upgrades/rollback, live snapshots or general HA.
