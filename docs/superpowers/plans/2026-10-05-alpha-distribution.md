---
title: Alpha distribution plan
description: Package a matched chart and validate actual release artifacts.
---

# Alpha distribution acceptance implementation plan

Use `superpowers:executing-plans` for the existing inline execution workflow with independent whole-PR review; complete the approved fourth Kubernetes-alpha delivery. No runtime/API/SDK behavior changes.

Goal: package a matching chart, validate actual distributable inputs with the existing cluster lifecycle journey, and prepare one reviewed candidate for publication.
Architecture: extend the existing Python candidate tools and Go opt-in fixture; consume standard Helm archives and Docker OCI import, with no generalized registry service or production abstractions.
Constraints: versions runtime/chart/TS0.1.0-alpha.2, Python0.1.0a2, Go module sdk/go/v0.1.0-alpha.2; clean tracked source; exact revision; external operator state/sandbox; explicit model simulation; unique own cleanup; private keys/payloads; same-release only.

Task1 — matched chart and image identity
Files: scripts/release/build.py, smoke.py, images.py and distribution tests; Makefile/tool installation where consumed.
- [ ] Independent regression: mismatched chart version/appVersion cannot create candidate; package has only tracked source and expected chart identity. Observe RED.
- [ ] Package snapshot chart via official Helm without version overrides; include helm-chart record in common checksums; inspect packaged metadata and render with independently authored operator values.
- [ ] Extend validated known Buildx OCI record with top-level image index digest; independently constructed OCI tests require both platforms and exact identity.
- [ ] Run distribution unit, chart/package checks and actual candidate build/inspection; commit.

Task2 — artifact installation/recovery
Files: scripts/kubernetes/candidate_test.go, cluster_test.go and source/fixture inputs.
- [ ] Independent regression: absent/old revision/changed bytes/path traversal/duplicate roles fail before importing images; native archive cannot extract outside its private target. Observe RED.
- [ ] Optional candidate-dir mode validates common manifest and hashes via bounded streaming, imports complete OCI archives using a containerd Docker image store, tags unique fixture aliases, inspects native architecture/UID/identity, extracts matching native supervisor, and uses packaged chart. Default source mode is retained only as a contributor test path, not product compatibility.
- [ ] Build only explicit simulated-model fixture from source; run unchanged authenticated restart, native sandbox and independent original-Temporal-history restore journey from actual artifacts.
- [ ] Record exact version/source/digests and cleanup result; commit.

Task3 — candidate CI, operator release instructions and merge
Files: .github/workflows/release-build.yml, candidate CI/tool install, release/deployment/SDK guides, capability/provenance records.
- [ ] Credential-free manual candidate workflow verifies standard tool downloads, exports both multi-platform images, finalizes manifest then runs artifact-mode cluster tier. Upload only after checks pass; no automatic publication.
- [ ] Document inspected archive installs, digest-pinned chart image, external supervisor, all backup boundaries and precise unsupported paths. Registry availability is only claimed after successful publication/retrieval.
- [ ] Run Actions/docs/lint/unit/race and affected service/package/live tiers as warranted; independent whole-PR review, fixes, exact-head required CI, clean merge with human changes preserved.
- [ ] Build/inspect final merged-revision candidate; publish exact validated artifacts/tags and verify public identity/retrieval. If publishing credentials are unavailable, preserve complete artifacts/evidence and report the concrete missing authorization, never introduce credentials as CI requirements.

Spec: `docs/design/alpha-distribution.md`; approved global scope: `docs/design/kubernetes-alpha.md`.
