---
title: Kubernetes alpha recovery plan
description: Exercise the real chart and external worker in isolated state stores.
---

# Kubernetes Alpha Recovery Plan

> Use superpowers:executing-plans inline, with independent whole-PR review. The maintainer authorized the complete alpha delivery and reviewed clean merges; no repeat approval for the accepted topology.

**Goal:** Establish exact same-release installation, restart and consistent restore evidence for the control-plane alpha.

**Architecture:** Go system tests own kind/Compose fixtures and ordinary public HTTP journeys. The production chart and existing native Docker supervisor are exercised unchanged. A small test-only Messages server supports deterministic replay.

**Spec:** `docs/design/kubernetes-recovery.md`, constrained by `docs/design/kubernetes-alpha.md`.

## Global Constraints

- External PG/Temporal/NATS/S3 fixtures, unique projects/cluster and explicit kubeconfig; never reset maintainer infrastructure.
- Test-only model endpoint, no real or hosted agent credentials; no reference SDK/cookbook execution.
- Real image commands and external Docker worker, numeric non-root containers and original keyring.
- Three quiesced database backups and independent S3 byte restore, same release only.
- Existing raw HTTP/SDK/runtime tests retain their responsibilities; no production orchestration abstractions.

## Review Focus

- Quiescence must include API, orchestration, external worker and Temporal before dumping either persistence store.
- Restore must use independent database/object-store volumes and the original keyring, then resolve a pending action through restored Workflow history.
- Test cleanup and diagnostics must target only exact owned names/IDs and never print credentials or backups.
- HTTP expectations must be independently authored; model fixture must validate actual tool results and derive replay from supplied transcripts.
- Actual worker connection must cross the documented Environment Work boundary; no host-process sandbox or mock execution.

## Task 1: Explicit Messages and state-service fixtures

**Files:** `scripts/kubernetes/model/main.go`, adjacent tests, fixture Dockerfile, `scripts/kubernetes/fixtures/compose.yaml`.

**Interfaces:** The fixture accepts `/v1/messages` with JSON or SSE responses; closed instructions select Bash or a custom tool. Compose consumes an explicitly named fixture image, pins existing service versions, bounds resources and exposes only a loopback ephemeral S3 port.

- [ ] Write tests for initial Bash/custom actions, resumed tool correlation, rejected erroneous results and SSE response assembly; observe RED.
- [ ] Implement the small stateless fixture; create separate Compose state services using no existing container names.
- [ ] Run fixture tests and Compose configuration validation; commit.

## Task 2: Real chart install and process replacement

**Files:** `scripts/kubernetes/cluster_test.go`, `journey_test.go`, `Makefile`.

**Interfaces:** `make test-kubernetes KIND=... KUBECTL=... HELM=...` enables the required Go system test, which creates its own cluster and two fixture projects. Shared HTTP helpers use independently authored request/response expectations. The native supervisor receives only Environment key and private fixture URLs.

- [ ] Author the fresh-install/File/Skill/Memory/confirmation journey and run against the new isolated topology; fail on missing setup or observable contract mismatch.
- [ ] Build images with explicit test version/revision, install chart with existing Secrets and real migration, connect external worker.
- [ ] Verify pending action survives API/orchestration replacement, duplicate result conflicts, worker completion flushes Memory and reactivation reads workspace/Skill/Memory.
- [ ] Verify exact owned cleanup on success/failure; run the opt-in tier and record evidence; commit.

## Task 3: Quiesced independent restore

**Files:** `scripts/kubernetes/recovery_test.go`, affected cluster/journey helpers.

**Interfaces:** Snapshot data stays in private test memory/files; pg_dump/pg_restore operate on three fixture databases. S3 copies carry exact bytes to a different fixture project; chart restore disables migration and retains original keyring. Public HTTP resolves the original pending custom action.

- [ ] Extend the test with pending custom action, captured immutable bytes/Memory Versions and encrypted fixture credential; observe missing restore failure.
- [ ] Quiesce all writers, dump/copy state, start independent stores and same-version Temporal, install second chart.
- [ ] Verify identities, bytes/checksums, history and resumed pending custom action; verify original keyring decryptability through a scoped outbound fixture workflow.
- [ ] Run the full cluster tier and relevant package/service checks; commit.

## Task 4: Required CI and honest operator evidence

**Files:** `.github/workflows/integration.yml`, CONTRIBUTING.md, Kubernetes/recovery/deployment guides, capabilities and provenance.

**Interfaces:** Required integration gate includes the cluster tier. Docs record precisely the exercised Kubernetes version, restart/restore paths, external backup responsibilities and unsupported paths; publication remains delivery4.

- [ ] Add pinned verified kind/kubectl/Helm installation and credential-free cluster acceptance to CI.
- [ ] Document actual commands and backup boundaries from observed evidence; run Actions/docs/Go/chart checks and configured live smoke when applicable.
- [ ] Independent whole-PR review, fix reproduced findings, create/attach PR, require exact-head CI and merge cleanly while preserving human primary edits.
