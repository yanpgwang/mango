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

The Mango application image serves three roles. Run the one-shot migration
before starting the independently scalable API and orchestration processes:

```text
mango migrate
mango serve -addr :8080
mango orchestrate
```

The default self-hosted tool path also needs a separate Environment worker.
Its supervisor runs `mango-worker docker`; its per-Work container executes
tools. See [Core concepts](concepts.md#environments-and-workers).

## Health probes

Use `GET /healthz` for API liveness and `GET /readyz` to decide whether to route
new requests to an API process. Both are public. Readiness queries the same
PostgreSQL pool used for requests, verifies writable transaction mode, and fails
with 503 after at most two seconds when the database cannot be reached or a
connection cannot be acquired. Recovery is checked on the next request.

Allow the external probe more than two seconds to receive Mango's failure
response. Do not restart the process just because readiness fails. The check is
a point-in-time observation: it does not test storage capacity or every write
permission. It does not certify Worker execution or model-provider availability.
Temporal and NATS outages do not change the running API's readiness because
PostgreSQL admits work durably for later processing. Startup still requires the
configured dependencies to connect.

## Workspace authentication

The API refuses to start without an active Workspace key. Set
`MANGO_API_KEY` to bootstrap or rotate the default Workspace key, or
manage additional Workspaces and keys through the operator CLI:

```sh
mango workspace create -name acme
mango api-key create -workspace wrkspc_... -label production
```

The plaintext generated key is printed only by `api-key create`; PostgreSQL
stores its SHA-256 digest. API and orchestration processes share Workspace
ownership through PostgreSQL. An external Environment supervisor receives a
key issued with `api-key create -workspace ID -environment ID -label LABEL`;
configure it as `MANGO_ENVIRONMENT_KEY`. See the
[worker guide](guides/self-hosted-worker.md#issue-a-supervisor-key).

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

Database migration is an explicit one-shot role. Normal API, orchestration,
Workspace, and API-key commands only read the migration ledger at startup;
they do not initialize or change the schema. The API's optional API-key
bootstrap still writes application data after this check succeeds.

Run `mango migrate` with `MANGO_DATABASE_URL` pointing to the intended database
before starting a new installation or after deploying a binary with pending
migrations. It needs only PostgreSQL; it does not connect to Temporal, NATS,
object storage, or a model. Concurrent migration jobs for the same database
schema serialize through a PostgreSQL advisory lock. Re-running the command
preserves existing application data and bootstrap Workspace changes.

Startup fails with `run mango migrate` guidance if the ledger is absent or a
required migration is pending. Applied versions absent from the binary are
rejected by both startup and migration; use a matching checkout/release and
database. The check validates applied migration versions, not manual schema
edits or a complete upgrade/rollback policy. A read-only connection can perform
the schema check, but the API's `/readyz` still rejects read-only PostgreSQL.

## Repository commands

### Development database baseline

The current schema starts from `internal/pg/migrations/00001_schema.sql`.
Historical pre-release migrations have been squashed into this baseline;
their old-row backfills and downgrade paths are no longer supported. Mango
has no supported stable release or customer database migration requirement.

Use a fresh development database when moving from a checkout that used the
historical migration chain. Do not edit Goose's version table to make an old
database appear initialized. Keep the old database and checkout together if
you need to inspect their data. There is no in-place upgrade path.
The baseline may also change directly during pre-release development. For
example, Skill Version expanded-size metadata must now be nonnegative; rebuild
an older development database before using this contract. The startup ledger
check does not detect changes made within an already applied baseline.

For a disposable local Compose stack, `make local-down VOLUMES=1` followed by
`make local-up` rebuilds state from the baseline. **This deletes all local
PostgreSQL data, Temporal history, and MinIO objects.** Back up anything you
need before using that reset. Contributor tests create isolated schemas and
do not require resetting a running local stack.

The local Compose stack builds the shared Mango image in its `migrate` service.
API and worker startup wait for that service to complete successfully. For
direct binary use, set `MANGO_DATABASE_URL`, run `mango migrate`, then create
operator keys or start the API and worker. Migration does not reset a database
from the historical chain or make it compatible with this baseline.

### Build and run

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
