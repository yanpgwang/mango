---
title: Kubernetes chart candidate
description: Install the Mango control plane using explicit operator dependencies and Secrets.
---

# Kubernetes chart candidate

This is delivery 2 of the maintainer-authorized
[Kubernetes alpha](kubernetes-alpha.md). The operator should configure existing
state services and Secrets, install one API and one orchestration process from
the same immutable image, and connect their own sandbox worker. Cluster recovery
and publication remain separate acceptance deliveries.

## Installation and state

`charts/mango` is a Helm application chart, version/appVersion
`0.1.0-alpha.2`. It installs two Deployments, an API ClusterIP Service, a
nonsecret ConfigMap, and a pre-install migration hook Job. It has no stateful
dependencies, Session Pods, Docker socket, cluster RBAC or sandbox launcher.
Image digest overrides take precedence over the explicit version tag; no
`latest` image selection exists. Existing image-pull Secrets may be selected.

The hook uses only a database Secret reference and `mango migrate`, including
the existing PostgreSQL migration lock. It does not need the ConfigMap or a
chart-created ServiceAccount before those resources exist. Successful hooks
are removed; failures remain available for diagnostics, with a one-hour TTL.
The Job has a five-minute deadline and bounded retries. API and orchestration
continue to check schema without writing it.

Before first runtime publication, rename the current consolidated baseline
from version `1` to the normal Goose timestamp version `20261005000001`.
DDL and seed contents remain unchanged. Existing version checks then reject
known older development databases before migration or serving; they never
rewrite their version table or reset data. Retrying this alpha's migration and
restoring a database from this same release remain idempotent. This is not a
schema checksum or detection of arbitrary manual edits. Do not change ledger
rows to bypass it. Fresh database/schema installation is required; there is no
upgrade reader, compatibility layer or reset command.

The default migration hook may be disabled explicitly when restoring the same
release's initialized database. Cross-version upgrades and rollback are not
supported. The chart does not automate external database/storage provisioning,
credential grants, TLS termination or backup.

## Configuration and trust

Values select existing Secrets and individual keys; no credential values or
Secret resources belong in the chart. Database and NATS URLs come from Secrets
because URLs may contain credentials. An optional migration database Secret
allows a distinct DDL role. PostgreSQL, Temporal address/namespace, NATS, an
existing S3 bucket/region/endpoint and S3 credential Secret are required.
The bucket must exist and state services must be available before installation.

Model endpoint, model ID and authentication mode are nonsecret configuration;
only orchestration receives the model credential Secret. A model endpoint is
required, preventing the runtime's empty-config offline model from becoming
the default installation. CI will use an explicitly simulated Messages endpoint.
An optional Vault keyring Secret mounts only its selected file into API and
orchestration, enabling Vaults/Webhooks with the original keys. No keyring is
generated or rotated by Helm. Runtime startup and existing diagnostics govern
external dependency availability; `/readyz` checks PostgreSQL writability, not
model, sandbox or every service's health.

Containers use UID/GID/fsGroup 65532, RuntimeDefault seccomp, no privilege
escalation, dropped capabilities and a read-only root filesystem. Each Pod has
an explicitly sized writable `/tmp` emptyDir and requests/limits including
ephemeral storage. Kubernetes service account tokens are not mounted. Model
credentials do not appear in API or migration Pods, and migration receives no
File, Vault or NATS credentials.

The default tested topology has one API and one orchestration replica. Worker
replacement uses Recreate and a 60-second termination grace. API grace is
15 seconds for its existing five-second HTTP shutdown. API startup/liveness
use `/healthz`; readiness uses `/readyz` with a three-second timeout, exceeding
the server's two-second database bound. Orchestration has no fabricated
provider/sandbox readiness probe; Kubernetes tracks its process and the next
delivery verifies real workflow consumption and termination.

## Acceptance and limits

Independent rendered-manifest tests must prove required configuration failures,
role-specific secret selection, equal image identity, migration-hook order,
read-only filesystem/writable temporary storage, probes, namespace/selector
consistency and bounded lifecycle. Digest selection and long resource names
must remain valid. Package/lint checks must include only chart distribution
inputs. Add a required CI check using Helm 4.3.0.

The documentation provides explicit existing-Secret configuration, an initial
installation walkthrough, external-worker connection, failure diagnostics,
original-key preservation and the fresh-database restriction. It labels this
as a chart candidate until isolated cluster/recovery and publication pass.
No HTTP/OpenAPI changes, bundled data services, Ingress, HPA, Operator, HA
claim or cloud sandbox are part of this slice.

## Design references

Helm v2 chart structure, values schemas and pre-install Job hooks, Kubernetes
probes/security contexts, and Goose's timestamp migrations are standard design
references. CMA is a hosted control plane; its self-hosted Environment boundary
informs separation of sandbox execution, not Mango's schema or chart lifecycle.
The adopted contract is Mango's independently operated deployment.
