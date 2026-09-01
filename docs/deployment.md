---
title: Deployment model
slug: /deployment
sidebar_position: 4
---

# Deployment model

Mango currently publishes a reproducible local stack and builds a multi-role
application image. It does not yet publish a supported production Docker
Compose bundle or Kubernetes chart.

## Supported assets

| Asset | Status | Intended use |
| --- | --- | --- |
| Root `Dockerfile` | Buildable | Produce the API/worker image on Linux AMD64 or ARM64 |
| `deployments/local/compose.yaml` | Development | Run PostgreSQL, Temporal, NATS, MinIO, OpenSandbox, API, and worker from the current checkout |
| `deployments/qualification/opensandbox-kata` | Qualification | Record and exercise the selected Kubernetes/Kata sandbox contract; does not install a cluster |
| Production Docker Compose | Planned | Supported single-host installation using versioned release images |
| Helm chart | Planned | Kubernetes API and worker deployments with external stateful dependencies |

The local stack is intentionally complete so contributors can exercise the
durable path without installing each dependency. It contains development
credentials, fixed host ports, and stateful dependencies and must not be
treated as a high-availability or hardened production configuration.

The OpenSandbox/Kata qualification directory is intentionally different from
a deployment bundle. It selects the production-candidate execution chain and
provides configuration templates plus an opt-in live gate, while leaving the
OpenSandbox, Kubernetes, and Kata release matrix under explicit operator
control. It must not be presented as a supported chart until that complete
matrix has repeatable upgrade and rollback evidence.

## Process topology

One immutable image serves two independently scalable roles:

```text
mango serve -addr :8080
mango orchestrate
```

The API refuses to start without an active Workspace key. Set
`MANGO_API_KEY` to bootstrap or rotate the default Workspace key, or
manage additional Workspaces and keys through the operator CLI:

```sh
mango workspace create -name acme
mango api-key create -workspace wrkspc_... -label production
```

The plaintext generated key is printed only by `api-key create`; PostgreSQL
stores its SHA-256 digest. API and worker processes share Workspace ownership
through PostgreSQL, but only the API needs request credentials.

The API owns HTTP resources, SSE, event admission, and client Files
metadata/object coordination. The worker owns Temporal Workflow/Activity
execution, model calls, sandbox tools, File Resource materialization, Session
output publication, and the outbox relay. They share a release artifact but
not a scaling or rollout policy.

Files add an S3-compatible dependency beside PostgreSQL, Temporal, and NATS.
Set `MANGO_FILE_S3_BUCKET` to enable the five Files routes; leaving it
empty keeps the rest of the API available and makes Files requests return
`422`. Failure to initialize or reconcile the configured object store also
disables only Files so the Mango core remains available. The API
process uses these settings for uploads and File-message admission. A worker
that starts Deployment Runs containing File messages or File outcome rubrics,
materializes Session File Resources, or publishes `/mnt/session/outputs` must
use the same bucket, endpoint, region, and credentials (it does not run startup
intent reconciliation):

| Variable | Meaning |
| --- | --- |
| `MANGO_FILE_S3_BUCKET` | Required bucket name; empty disables Files |
| `MANGO_FILE_S3_REGION` | AWS region; defaults to `us-east-1` |
| `MANGO_FILE_S3_ENDPOINT` | Optional S3-compatible endpoint |
| `MANGO_FILE_S3_ACCESS_KEY` / `MANGO_FILE_S3_SECRET_KEY` | Optional static credentials; configure both together |
| `MANGO_FILE_S3_PATH_STYLE` | Use path-style addressing for providers such as MinIO |
| `MANGO_FILE_S3_CREATE_BUCKET` | Development convenience; create a missing bucket |
| `MANGO_FILE_UPLOAD_TEMP_DIR` | Directory for bounded upload spool files |

The first Files slice assumes one Files-enabled API process during startup
reconciliation. It also needs temporary disk capacity up to 500 MB per
concurrent upload, Session Resource copy, or Session output publication. These
are explicit limits until distributed intent leasing and direct multipart
object-store operations are implemented.

File-backed Session Resources, Git worktrees, Session Outputs, Skills, and
Memory attachments all use OpenSandbox. The sandbox image must provide
`/bin/sh`, `tar`, and the POSIX utilities documented in the
[sandbox runtime guide](sandboxes.md#runtime-image-contract). The API and every
worker on the task queue must use the same object-store configuration. There is
no provider selector or host-process/direct-Docker backend.

Memory API contents and immutable Versions live in PostgreSQL and do not
require S3-compatible storage. An attached Store is one OpenSandbox-managed
volume mounted publicly as read-only or read-write and privately for trusted
Mango refresh/writeback. OpenSandbox supplies mount isolation; PostgreSQL
remains canonical.

### OpenSandbox worker configuration

Every worker requires `OPEN_SANDBOX_DOMAIN`; production normally also requires
`OPEN_SANDBOX_API_KEY` and `OPEN_SANDBOX_USE_SERVER_PROXY=true`. Select the
reviewed sandbox image with `OPEN_SANDBOX_IMAGE`. The API does not need
OpenSandbox credentials because capability admission is a fixed property of
this Mango build. A healthy API alone does not establish worker or sandbox
readiness.

The local Compose bundle starts a pinned OpenSandbox service on loopback. Only
that service mounts the Unix socket selected by `OPEN_SANDBOX_DOCKER_SOCKET`; Mango API
and worker containers remain non-root and have no Docker socket, host resource
bind, or daemon credential. Session containers never receive model
credentials. The Docker runtime shares the host kernel and is not a hardened
hostile multi-tenant boundary.

OpenSandbox persists its local control-plane database in the
`opensandbox-data` volume. Its Session containers and managed volumes survive a
Mango worker restart and are reattached through persisted bindings. Session
deletion releases both. Stop and delete Sessions through Mango before
discarding OpenSandbox state; removing only Mango's database can orphan
external resources.

Production moves the same Mango integration to a private authenticated
OpenSandbox service using Kubernetes BatchSandbox and a reviewed Kata
RuntimeClass. Mango does not need a Docker socket or Kubernetes credentials in
that topology.

The Vault and Webhook APIs are disabled unless `MANGO_VAULT_KEYRING_FILE` points to
an operator-mounted JSON keyring. A configured but invalid keyring fails API
startup rather than falling back to plaintext storage. The file has this shape:

```json
{
  "active_key_id": "2026-08",
  "keys": {
    "2026-08": "<standard-base64 32-byte AES key>",
    "2026-07": "<retained decrypt-only key>"
  }
}
```

New Credentials, Webhook signing secrets, and secret/auth updates use the active key. Older keys may remain in
the file for reads during rotation; removing one makes Credentials encrypted by
that key unavailable. Both the API and worker processes must load the same
keyring: the API encrypts and admits Session Vault references, while workers
decrypt matching credentials immediately before MCP requests and Webhook
delivery. It must never be
mounted into a Session sandbox, copied into Agent context, or stored in
PostgreSQL. The bundled local keyring is
deterministic development material and must not be reused outside the local
Compose stack.

Before production deployment bundles are promoted, database migration will be
removed from normal API/worker startup and exposed as an explicit one-shot
role. This avoids every replica racing to manage schema during a rollout.

## Repository commands

Run core checks:

```sh
make verify
```

Build and smoke-test the container entrypoint:

```sh
make image-smoke
```

Builders behind a restricted network can pass a standard Go module proxy
without changing the Dockerfile:

```sh
make image-smoke GOPROXY=https://proxy.example.com,direct
```

Validate and start the local stack:

```sh
make local-config
make local-up
make local-health
```

Stop it while retaining PostgreSQL and MinIO data:

```sh
make local-down
```

Set `VOLUMES=1` only when all local state should be removed:

```sh
make local-down VOLUMES=1
```

## Production promotion gates

A supported Docker or Kubernetes bundle requires:

1. explicit, versioned schema migration;
2. dependency-aware API and worker readiness;
3. graceful API shutdown and worker draining;
4. repeatable live conformance for the OpenSandbox Docker and Kubernetes/Kata profiles;
5. real PostgreSQL, Temporal, NATS, S3-compatible storage, and sandbox
   integration tests in CI;
6. distributed Files reconciliation and documented temporary-disk sizing;
7. versioned images with upgrade and rollback documentation.

For the selected OpenSandbox/Kata sandbox path, promotion additionally requires
the committed live qualification against the exact pinned Kubernetes, Kata,
OpenSandbox server/controller, `execd`, egress, and sandbox-image matrix. A
successful OpenSandbox Docker test does not satisfy the isolation gate.

Kubernetes packaging will use separate API and worker Deployments from the same
image. Stateful services remain external by default. An Operator is not part
of the initial deployment model and will be considered only if Mango introduces
Kubernetes-native custom resources that require reconciliation.
