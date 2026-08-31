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
| `deployments/local/compose.yaml` | Development | Run PostgreSQL, Temporal, NATS, MinIO, API, and worker from the current checkout |
| Production Docker Compose | Planned | Supported single-host installation using versioned release images |
| Helm chart | Planned | Kubernetes API and worker deployments with external stateful dependencies |

The local stack is intentionally complete so contributors can exercise the
durable path without installing each dependency. It contains development
credentials, fixed host ports, and stateful dependencies and must not be
treated as a high-availability or hardened production configuration.

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

File-backed Session Resources require `MANGO_SANDBOX=docker`, `e2b`, `cube`,
`opensandbox`, or `daytona`; the remote providers currently expose writable
sandbox-local copies. Automatic Session output publication supports the same
providers. Every remote image must provide `/bin/sh` and `tar`; output archives
are removed after each snapshot. OpenSandbox and Daytona stream file transfers,
while E2B and Cube buffer each File Resource and output archive in worker
memory, so operators must provision memory for the largest accepted transfer.
A Docker worker must run where the selected Docker
Engine API is reachable; the provider uses the Moby Go client directly and does
not require a `docker` CLI binary. Configure a non-default daemon with
`DOCKER_HOST` and the standard Docker TLS environment variables. The daemon
must be able to bind the worker's provider-owned staging directory. Set
`MANGO_SANDBOX_RESOURCE_DIR` to place that directory on a dedicated
host volume; the default is `mango-resources` beneath the process
user's home directory. The API and every worker
on the task queue must agree on the sandbox provider and object-store
configuration. Host-process execution is not a selectable runtime backend.

Memory API contents and immutable Versions live entirely in PostgreSQL and do
not require S3-compatible storage. Memory-backed Session Resources do require
`MANGO_SANDBOX=docker`: the API snapshots each attachment, and the
worker bind-mounts it beneath `/mnt/memory`, then synchronizes tool changes back
to PostgreSQL. API and worker processes must select the same provider.
Self-hosted and current remote adapters reject Memory Store attachment while
the standalone Memory API remains available.

### Docker worker configuration

Docker is the default for native processes and the local Compose stack. An
unreachable daemon fails worker startup; `MANGO_SANDBOX=local` is rejected and
the former unsafe-local override has no effect. API startup reads the Docker
capability declaration without needing daemon access. A healthy API alone does
not establish worker or sandbox readiness.

The Compose worker is a trusted daemon controller. It runs as root and mounts
`/var/run/docker.sock`, or the host Unix socket selected by `MANGO_DOCKER_SOCKET`.
This grants substantial host authority; do not expose the socket to untrusted
users. The API has no socket. Session containers receive their own input,
output, Skill, and Memory mounts, never the daemon socket or model credentials.
They share the host kernel and are not a hardened hostile multi-tenant boundary.

`MANGO_SANDBOX_RESOURCE_DIR` defaults to `$HOME/mango-resources` in Compose.
Its bind source and target are the same absolute path so the worker and host
daemon resolve generated mount paths identically. A custom value must be an
absolute directory visible to the daemon; Docker Desktop must share that path.
Mounting only the socket is insufficient. The default image is
`python:3.12-alpine`; choose `MANGO_SANDBOX_IMAGE` when other runtimes or package
managers are required. Alpine does not include `apt-get`.

Restart retains the Session container and reattaches through its persisted
binding. Missing containers fail explicitly instead of creating empty
replacement workspaces. Session deletion releases its container and staged
resources. `make local-down` stops Compose services but deliberately retains
Session containers and their host directory; it is not Session deletion.
Delete Sessions through Mango before discarding deployment data. Even
`VOLUMES=1` does not remove sibling Session containers or host bind directories.

An older local-backed Session cannot resume on this Docker worker. Finish and
delete those Sessions with the old worker before upgrading; the new binary
neither migrates their workspaces nor supports a local compatibility executor.
No development database or existing workspace is automatically erased.

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
4. repeatable live conformance for remote sandbox adapters;
5. real PostgreSQL, Temporal, NATS, S3-compatible storage, and sandbox
   integration tests in CI;
6. distributed Files reconciliation and documented temporary-disk sizing;
7. versioned images with upgrade and rollback documentation.

Kubernetes packaging will use separate API and worker Deployments from the same
image. Stateful services remain external by default. An Operator is not part
of the initial deployment model and will be considered only if Mango introduces
Kubernetes-native custom resources that require reconciliation.
