---
title: Kubernetes alpha delivery
description: Release scope and acceptance for installing the Mango control plane on Kubernetes.
---

# Kubernetes alpha delivery

The maintainer selected Kubernetes alpha delivery on 2026-10-05. Users should
install a versioned Mango control plane, connect their own state services and
sandbox worker, and use matching first-party SDKs without building Mango.
Cloud sandbox support is an independent execution option and does not gate
this release. Agent/Deployment archive propagation remains ordinary feature
work, not a prerequisite for installation.

## First release scope

- API and Temporal orchestration run as separate Kubernetes Deployments.
- PostgreSQL, Temporal, NATS and S3-compatible storage are operator-provided.
  The chart does not install them. Test fixtures may provision them explicitly.
- The operator provides sandbox execution through Environment Work. The Docker
  launcher is a reference component outside the chart, not a control-plane
  dependency. The chart never mounts a Docker socket or creates Session Pods.
- The first release supports fresh installation and restart/recovery on the
  documented topology. Cross-version upgrades, rollback of persisted state,
  rolling Workflow versioning and general HA certification are not promised.
- Files/Skills need configured S3 storage; Vaults/Webhooks need the operator's
  encryption keyring. Model credentials belong only to orchestration.
- Runtime and chart use `0.1.0-alpha.2`, matching TypeScript
  `0.1.0-alpha.2`, Python `0.1.0a2`, and Go module tag
  `sdk/go/v0.1.0-alpha.2`. This is an alpha, not a supported stable API.

## Delivery sequence

1. **Release artifacts.** Build versioned binaries and independently installable
   SDK artifacts from one clean Git revision, with a manifest and checksums.
   Build both OCI images for Linux AMD64 and ARM64, with matching version and
   revision metadata. Manual candidate builds do not publish automatically.
2. **Helm chart.** Explicit external dependency configuration, existing Secrets,
   one-shot initial schema Job, API Service, probes, resource limits, temporary
   upload storage and bounded termination. No default credentials or silent
   installation into an old development database.
3. **Cluster conformance and recovery.** Install into an isolated kind cluster;
   exercise authenticated HTTP, persisted waits, API/orchestration restart,
   and an operator-run Docker worker. Verify Files/Skills and Memory survive
   replacement. Default CI uses no provider credentials; run the configured
   live-model smoke separately when available.
4. **Release acceptance and publication.** Record exact image digests and SDK
   artifacts, installation and external-worker instructions, supported recovery
   evidence, backup responsibilities and unsupported upgrades. Publish only the
   artifacts from the reviewed revision after required checks pass.

Each delivery is a focused PR with independent review and passing CI before
merge. This document defines the complete release objective; each PR has its
own implementation plan and acceptance evidence.

## Release integrity

The candidate builder rejects an invalid version, mismatched SDK version,
dirty source checkout, revision mismatch or existing output directory. Its
manifest identifies the source revision and every archive's SHA-256. Archives
contain allowlisted distribution contents; local credentials, build caches,
test environments and cookbook data are not distribution inputs.

The `mango version` and `mango-worker version` commands report their version
and full revision without connecting to any service. Development builds report
`dev` and `unknown`. OCI metadata identifies the same pair. The reference
worker's release invocation selects the matching published item image; operator
image overrides remain available.

Fresh wheel, source-distribution and npm installs must support public imports,
typed resource access and authenticated request/response handling. The Go SDK
source artifact must build in a fresh module. Existing raw HTTP and SDK
conformance remain independent of artifact building.

## Kubernetes lifecycle

The schema Job uses only the database credential and runs `mango migrate`.
Normal serving and orchestration only check the ledger. Required configuration
must fail clearly rather than producing an unauthenticated or fake-model
installation. The integration fixture explicitly selects its deterministic
model; normal installation requires configured model settings.

API liveness uses `/healthz`; readiness uses `/readyz` with a probe timeout
longer than its two-second PostgreSQL deadline. NATS/Temporal interruption must
not discard accepted input. Orchestration startup and termination must be
tested; process liveness is not represented as sandbox or provider readiness.

Secrets are selected by role and mounted only where needed. Sandbox processes
receive only scoped Work credentials. Containers run as a numeric non-root
user, drop capabilities, disable privilege escalation and use a read-only root
filesystem with explicit writable temporary storage. API exposure is a
ClusterIP by default; ingress and TLS termination remain operator choices.

## State and recovery

PostgreSQL contains Mango metadata, event history, Memory versions, credentials
and coordination facts. Temporal separately owns Workflow history. S3 owns
File/Skill bytes; PostgreSQL metadata alone cannot reconstruct them. The Vault
keyring must accompany encrypted data. User sandbox working directories are
operator-owned and outside chart deletion and Session deletion guarantees.

Installation documentation must explain these backup boundaries and preserve
original encryption keys. Restore validation must use a consistent, quiesced
fixture into independent state stores; it must not reset the maintainer's local
stack. NATS Core previews/wakeups are ephemeral and repaired from durable state.
An alpha release records exactly which restart and restore paths were exercised;
it does not claim arbitrary live snapshots or production HA.

## Research and intentional choices

CMA's public Environment design distinguishes cloud and self-hosted execution;
the maintainer selected only the latter for Mango's first distribution. CMA is
a hosted control plane, so its rollout and beta headers do not define a Mango
Kubernetes release. Mango's existing `/v1`, SDKs and Environment Work protocol
remain the product contract. Helm chart conventions, Kubernetes Jobs and probes,
and OCI image metadata inform packaging; none delegate runtime behavior.

No cloud/OpenSandbox integration, image/PDF input, Kubernetes sandbox launcher,
Operator, bundled state services, compatibility shim or feature-parity audit is
part of this release.
