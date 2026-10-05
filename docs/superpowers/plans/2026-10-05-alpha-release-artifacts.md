---
title: Alpha release artifacts implementation plan
description: Build matched runtime and SDK candidates from a reviewed source revision.
---

# Alpha Release Artifacts Implementation Plan

> **For agentic workers:** Use `superpowers:executing-plans` task by task. The maintainer has authorized implementation, independent subagent review, CI and merge; do not repeat that approval request.

**Goal:** Produce inspectable runtime and SDK release candidates without source builds by end users or automatic publication on merge.

**Architecture:** A small shared build-info package supplies CLI/OCI identity. A standalone distribution builder invokes existing Go/npm/uv tools, allowlists archives and writes a version/revision manifest with checksums. A manual CI workflow builds candidates from clean source; final publication follows the later chart and cluster acceptance PRs.

**Tech Stack:** Go, Python 3.12+ standard library, npm, uv, Docker Buildx and GitHub Actions.

**Spec:** `docs/design/kubernetes-alpha.md`, release-artifact section. This plan covers delivery 1 only; chart, cluster/restore and final publication have separate plans.

## Global Constraints

- Runtime/TypeScript `0.1.0-alpha.2`, Python `0.1.0a2`, Go tag `sdk/go/v0.1.0-alpha.2`.
- Linux AMD64/ARM64 and Darwin AMD64/ARM64 command archives; OCI images are Linux AMD64/ARM64.
- Build only clean tracked source with exact full Git revision; never overwrite a candidate directory.
- Distribution excludes local credentials, caches, test environments and cookbook data.
- No automatic publication, new hosted credentials, HTTP changes or schema changes in this PR.
- Preserve existing main-checkout human edits and running local infrastructure.

## Review Focus

- A release binary reports its injected version/revision rather than unused linker variables.
- Platform selection cross-compiles both commands correctly; the worker image compiles on the build platform for the target.
- A partial SDK build or version mismatch cannot leave an apparently complete manifest.
- Dirty/untracked source and an existing candidate destination fail without deleting operator files.
- Fresh SDK installs resolve exported types and resource calls without checkout-relative imports.

## Task 1: Shared version identity and image builds

**Files:** Create `internal/buildinfo/buildinfo.go`, tests; modify command dispatch in `cmd/mango/main.go` and `cmd/mango-worker/main.go`, both Dockerfiles, SDK Go version/User-Agent, and build flags in Makefile.

**Interfaces:** `buildinfo.Version`, `buildinfo.Revision` default to `dev`/`unknown`; `buildinfo.Write(io.Writer) error` writes one JSON object with `version` and `revision`. Both commands expose `version` without infrastructure connections.

- [ ] Write the failing writer/command tests: JSON decodes to the injected pair, write failures propagate, and version command requires no credentials. Name literal outputs independently.
- [ ] Run `go test ./internal/buildinfo ./cmd/mango-worker ./cmd/mango`; observe missing behavior.
- [ ] Implement the shared metadata and command dispatch. Inject the same package variables into native and OCI builds; use numeric UID/GID 65532 and correct BuildKit target-platform arguments.
- [ ] Check actual native builds with explicit linker values and `version`; build/run the control-plane image's version command and build the reference worker image.
- [ ] Run `go test ./internal/buildinfo ./cmd/mango-worker ./cmd/mango` and Go SDK tests; commit the complete slice.

## Task 2: Clean candidate builder and installed-package checks

**Files:** Create `scripts/release/build.py`, `scripts/release/test_build.py` and package smoke script; update Makefile and ignore generated candidate output.

**Interfaces:** `python3 scripts/release/build.py --version 0.1.0-alpha.2 --output DIR [--platform OS/ARCH ...]` builds both commands, Go SDK source, Python wheel/sdist and npm tarball, then writes `manifest.json` and `SHA256SUMS`. Defaults select all four command platforms. No registry publication.

- [ ] Write failing tests for invalid version/revision, SDK mismatch, dirty source and existing output, archive allowlists, checksum/manifest completeness and failed subprocess cleanup.
- [ ] Run `python3 -m unittest discover -s scripts/release -p 'test_*.py'`; observe missing behavior.
- [ ] Implement validation before mutation, isolated staging, argument-array subprocesses and atomic final-directory publication. Use current package tools rather than reimplementing SDK packaging.
- [ ] Add `make release-build` and `make release-check`; build from a clean committed candidate revision and exercise version commands plus fresh wheel/sdist/npm/Go-source installs.
- [ ] Run the release unit tests, fresh installation smoke and existing SDK tests/conformance. Commit and record the actual artifact checksums and inspected contents.

## Task 3: Candidate CI workflow and operator documentation

**Files:** Create `.github/workflows/release-build.yml`, release guide; update `.github/workflows/ci.yml`, deployment and SDK documentation/provenance as applicable.

**Interfaces:** Required CI `Release artifacts` verifies package-building tests and fresh candidate installs. Manual `Release candidate` builds immutable candidate archives and both multi-platform OCI images without publishing; each artifact identifies the checked-out revision.

- [ ] Configure candidate-only workflow permissions; no credentials or package-write role in pull-request CI. Never execute untrusted input through shell interpolation.
- [ ] Validate Actions syntax, build both Linux image targets, run required Go/service/SDK/docs checks and configured self-hosted live smoke for the worker image changes.
- [ ] Document candidate build/install/inspection and the distinction from an actual release. Keep public package status source-only until registry publication is verified.
- [ ] Request independent whole-PR review; reproduce/fix material findings, create and attach the focused PR, require CI at the exact head, then merge.

Subsequent PRs implement chart, cluster/restore validation and publication from the final reviewed release revision. This candidate PR does not claim Kubernetes support or alpha publication.
