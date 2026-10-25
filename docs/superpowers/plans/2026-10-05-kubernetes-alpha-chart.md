---
title: Kubernetes chart implementation plan
description: Install two Mango control-plane roles against operator-owned state services.
---

# Kubernetes Chart Implementation Plan

> **For agentic workers:** Use `superpowers:executing-plans` task by task, with native implementation and independent whole-PR review. The maintainer authorized alpha delivery, review, CI and clean merge; do not repeat approval requests for these scoped implementation choices.

**Goal:** Provide an inspectable Helm chart candidate for fresh installation of Mango's control plane.

**Architecture:** Existing Mango commands and configuration are mapped directly into two Deployments and an initial schema Job. External Secret references are selected by role; standard Kubernetes security and lifecycle primitives carry operational policy. The consolidated Goose baseline receives the first release's distinct timestamp version without new persistence abstractions.

**Tech Stack:** Helm 4.3.0, Kubernetes apps/v1/batch/v1/core/v1, Go tests with the existing YAML library, PostgreSQL/Goose.

**Spec:** `docs/design/kubernetes-chart.md`, constrained by `docs/design/kubernetes-alpha.md`.

## Global Constraints

- Chart/appVersion `0.1.0-alpha.2`; runtime image shared across all roles, optional digest pin.
- Baseline `20261005000001`, identical DDL; reject old version `1` without modifying application state.
- External PostgreSQL, Temporal, NATS, S3 bucket and existing Secrets; no generated credential defaults.
- API/model secret isolation, optional original Vault keyring, migration database credential only.
- UID/GID/fsGroup 65532, read-only root, writable bounded `/tmp`, dropped capabilities and no service-account token.
- Fresh install/same-release restart only; no upgrade, cloud sandbox, Session Pods or bundled dependencies.

## Review Focus

- A pre-install hook must not depend on ConfigMap/ServiceAccount resources Helm has not yet created.
- Old same-number development schema must fail, while repeated initial migration preserves operator data.
- Missing required values fail rendering rather than select fake model or default credentials.
- Secret role leakage and keyring defaultMode/fsGroup must not expose unreadable or unnecessary credentials.
- Digest overrides and maximum-length Helm release names must produce valid consistent resources.

## Task 1: Distinct initial alpha schema version

**Files:** Rename `internal/pg/migrations/00001_schema.sql` to `20261005000001_schema.sql`; modify `internal/pg/migrations_integration_test.go`, CONTRIBUTING.md and deployment baseline notes.

**Interfaces:** Existing `pg.Migrate`/`pg.CheckSchema` consume the new embedded Goose source version; no signature or wire changes. Old applied version `1` is unsupported and cannot trigger DDL.

- [ ] Add `TestMigrateRejectsPreAlphaBaselineLedger`: replace the fixture's applied version with literal `1`; Migrate and CheckSchema must reject it while preserving ledger rows, Session and renamed Workspace.
- [ ] Run this real PostgreSQL regression before renaming; observe old version accepted.
- [ ] Rename the baseline without changing bytes; update rollback/reapply tests to literal `20261005000001` and update fresh-database documentation.
- [ ] Run migration/schema/command service tests, SQL generation drift checks and unit lint; commit the complete slice.

## Task 2: Chart resources and independent manifest checks

**Files:** Create `charts/mango/Chart.yaml`, `values.yaml`, `values.schema.json`, `.helmignore`, LICENSE, templates `_helpers.tpl`, configmap, API/orchestration deployments, Service, migration Job and NOTES. Create `scripts/helm/chart_test.go`, fixture values; add `make chart-check HELM=...`.

**Interfaces:** Values expose image repository/tag/digest/pullSecrets; database existingSecret and optional migrationSecret; auth existingSecret; Temporal address/namespace; NATS existingSecret; Files endpoint/region/bucket/pathStyle/existingSecret; model baseURL/ID/auth/existingSecret; optional Vault keyring; role resources and temporary storage. `chart-check` runs offline Helm lint/template/package contracts.

- [ ] Author independent rendered-manifest expectations for the two command roles, secret keys, equal images, hook-only DB env, standard security/temporary storage and API health/ready probes. Add rejection cases for empty config/typos and an unreadable or missing keyring mapping.
- [ ] Run chart tests before templates exist; observe missing chart behavior.
- [ ] Implement direct configuration mapping and schema validation. Use selected secretKeyRefs and ConfigMap keys, no envFrom credentials. Hook is pre-install only with existing DB/pull Secrets, before-hook-creation/hook-succeeded policies, 300-second deadline, three retries and 3600-second TTL.
- [ ] Add digest precedence and long-name uniqueness cases, optional migration role and keyring mounts, migration-disabled restore and chart package allowlist checks.
- [ ] Run strict Helm lint and chart contracts; commit the complete slice.

## Task 3: Required CI and operator walkthrough

**Files:** Update CI workflow, docs/deployment.md, deployments/README.md, docs/capabilities.md, docs/provenance.md and CONTRIBUTING.md; create chart README and Kubernetes guide.

**Interfaces:** CI downloads/verifies official Helm 4.3.0, then runs `make chart-check`. Docs use existing Secret names/keys and explicit dependencies; chart candidate/publication status stays honest.

- [ ] Document a fresh installation, external worker connection, configured model requirement, original keyring preservation and failure inspection; no destructive reset of the maintainer's stack.
- [ ] Validate Actions, chart package/rendering, docs, Go/SQL drift and affected service tests. Run configured self-hosted live smoke for the schema baseline change when available.
- [ ] Request independent full-PR review, address reproduced findings, create/attach the focused PR, require exact-head CI and merge when clean.

Delivery 3 will install this chart into isolated kind state services and verify workflow consumption, restart, Files/Skills/Memory and quiesced restore. This chart PR alone does not publish the alpha or claim Kubernetes support.
