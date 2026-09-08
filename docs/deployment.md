---
title: Deployment
description: Configure Mango’s processes, storage, credentials, and local development stack.
slug: /deployment
---

# Deployment

Mango currently publishes a reproducible local stack and builds a multi-role
application image. It does not yet publish a supported production Docker
Compose bundle or Kubernetes chart.

## Supported assets

| Asset | Status | Intended use |
| --- | --- | --- |
| Root `Dockerfile` | Buildable | Produce the API/worker image on Linux AMD64 or ARM64 |
| `deployments/local/compose.yaml` | Development | Run PostgreSQL, Temporal, NATS, MinIO, API, and worker from the current checkout |
| `deployments/self-hosted/docker` | Preview | Build and run the standalone Docker Environment Work supervisor and item image |
| Production Docker Compose | Planned | Supported single-host installation using versioned release images |
| Helm chart | Planned | Kubernetes API and worker deployments with external stateful dependencies |

The local stack is intentionally complete so contributors can exercise the
durable path without installing each dependency. It contains development
credentials, fixed host ports, and stateful dependencies and must not be
treated as a high-availability or hardened production configuration.

The self-hosted Docker worker is a separate target execution path. Its trusted
supervisor controls Docker and Polls/Acks Environment Work; each activation
runs in a fresh container with only the short-lived Work secret and a retained
per-Session workspace volume. It currently supports the six core shell/file
tools, immutable custom Skill preparation, and attached Memory Store
synchronization. File/Git resource preparation and output publication remain
open. Follow the
[Docker worker guide](guides/self-hosted-worker.md) to add it to a running control plane.

## Process topology

The Mango application image serves two roles. The local Compose stack runs
them in separate containers:

```text
mango serve -addr :8080
mango orchestrate
```

The default self-hosted tool path also needs a separate Environment worker.
Its supervisor runs `mango-worker docker`; its per-Work container executes
tools. See [Core concepts](concepts.md#environments-and-workers).

## Workspace authentication

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

Model credentials belong to the orchestration worker. Configure an endpoint
with the [model guide](guides/model-configuration.md); application clients and
Environment workers use Mango credentials instead.

## Object storage

Files add an S3-compatible dependency beside PostgreSQL, Temporal, and NATS.
Set `MANGO_FILE_S3_BUCKET` to enable the five Files routes; leaving it
empty keeps the rest of the API available and makes Files requests return
`422`. Failure to initialize or reconcile the configured object store also
disables only Files so the Mango core remains available. The API
process uses these settings for uploads and File-message admission. A worker
that resolves File message content or File-backed outcome rubrics must use the
same bucket, endpoint, region, and credentials (it does not run startup intent
reconciliation). Session workspace staging and output retrieval remain
operator-owned self-hosted concerns:

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
concurrent upload. These are explicit limits until distributed intent leasing
and direct multipart object-store operations are implemented.

The API and Temporal orchestration worker do not need Docker credentials. Run
the standalone Environment worker where its selected Docker Engine is
reachable. Configure a non-default daemon with `DOCKER_HOST` and standard
Docker TLS variables on that worker only. Host-process execution is not a
selectable runtime backend.

## Memory storage

Memory API contents and immutable Versions live entirely in PostgreSQL and do
not require S3-compatible storage. The self-hosted Docker worker downloads and
reconciles attached Stores through scoped Session APIs; its `/mnt/memory` tree
is a bounded tmpfs. Provider-specific launcher examples remain future work.

## Docker Environment worker

The standalone supervisor is a trusted Docker daemon controller. Session
containers never receive the daemon socket, Workspace credential, or model
credential. They share the host kernel and are not a hardened hostile
multi-tenant boundary.

The launcher owns named workspace volumes and their retention. Deleting or
archiving a Mango Session fences control-plane work but does not claim to erase
operator-owned storage. Follow the [worker guide](guides/self-hosted-worker.md)
for configuration and cleanup.

## Vault and Webhook encryption

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

Build the application image with `make image`. For a restricted build network,
set `GOPROXY` to an accessible Go module proxy. Contributor validation commands
are documented in [CONTRIBUTING.md](https://github.com/yanpgwang/mango/blob/main/CONTRIBUTING.md).

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
4. repeatable live conformance for supported Environment launchers;
5. real PostgreSQL, Temporal, NATS, S3-compatible storage, and worker
   integration tests in CI;
6. distributed Files reconciliation and documented temporary-disk sizing;
7. versioned images with upgrade and rollback documentation.

Kubernetes packaging will use separate API and worker Deployments from the same
image. Stateful services remain external by default. An Operator is not part
of the initial deployment model and will be considered only if Mango introduces
Kubernetes-native custom resources that require reconciliation.
