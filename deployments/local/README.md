# Local development stack

This directory builds and runs the complete local control plane with pinned
infrastructure versions and health checks.

| Service    | Image                          | Ports                       | Purpose                                              |
| ---------- | ------------------------------ | --------------------------- | ---------------------------------------------------- |
| PostgreSQL | `postgres:17.5-alpine`         | `5432`                      | Application event ledger, projections, admission outbox (and Temporal's own persistence, on separate databases). |
| Temporal   | `temporalio/auto-setup:1.29.7` | `7233` (gRPC)               | Durable session/thread orchestration.                |
| Temporal UI| `temporalio/ui:2.52.1`         | `8233` → container `8080`   | Workflow explorer at <http://localhost:8233>.        |
| NATS Core  | `nats:2.11.17-alpine`          | `4222` (client), `8222` (monitoring) | Ephemeral previews and SSE wakeups; PostgreSQL cursor reads repair loss. |
| SeaweedFS | `chrislusf/seaweedfs:4.48` (digest pinned) | `9000` → container `8333` | S3-compatible File and Skill bytes for development and service conformance. |
| API        | `mango:local`        | `8080`                      | PostgreSQL-backed Mango HTTP API. |
| Worker     | `mango:local`        | —                           | Temporal worker and PostgreSQL outbox relay. |

The Go module pins the matching client libraries: `go.temporal.io/sdk`,
`github.com/jackc/pgx/v5`, `github.com/pressly/goose/v3`, and
`github.com/nats-io/nats.go`. See the root `go.mod` for exact versions.

## Startup

From the repository root:

```sh
make local-up       # start everything in the background
make local-health   # block until all services are healthy
```

When `~/.config/mango/dev.env` exists, `make local-up` loads it via
`scripts/with-dev-env`. When the model variables are configured, the Compose
worker uses the real Messages endpoint. A missing file or empty model values
keep the offline deterministic model.

For an explicitly offline startup that bypasses the development file, use the
command in [Getting started](../../docs/getting-started.md#run-the-server).
Compose first runs the one-shot `migrate` service against PostgreSQL, then
starts Mango's API and Temporal orchestration worker. It does not
start an operator-owned Environment worker. To execute shell or file tools in a
default `self_hosted` Environment, separately start the
[Docker self-hosted worker](../self-hosted/docker/README.md) for that Environment.

The Compose orchestration worker does not mount the Docker socket and does not
own Session compute. The separately launched Environment worker owns its Docker
credentials, workspace volumes, and container lifecycle.

The API bootstraps `sk-mango-local-development` for the default Workspace.
Override it with `MANGO_API_KEY` before `make local-up`; never reuse the
bundled value outside local development.

`make health` returns only once Postgres accepts connections, the Temporal
frontend answers `cluster health`, NATS `/healthz` is green, SeaweedFS answers its
S3 `/healthz` probe, the API answers `/readyz`, and the worker process is alive.

Without `make`:

```sh
scripts/with-dev-env docker compose -f deployments/local/compose.yaml up -d --build
docker compose -f deployments/local/compose.yaml ps
```

## Connection strings

```sh
# Application database (pgx / goose / sqlc)
export MANGO_DATABASE_URL="postgres://postgres:postgres@localhost:5432/mango?sslmode=disable"
export MANGO_API_KEY="sk-mango-local-development"

# Temporal frontend (Go SDK client)
export MANGO_TEMPORAL_HOSTPORT="localhost:7233"
export MANGO_TEMPORAL_NAMESPACE="default"

# NATS Core live channel
export MANGO_NATS_URL="nats://localhost:4222"

# Files API object store
export MANGO_FILE_S3_ENDPOINT="http://localhost:9000"
export MANGO_FILE_S3_REGION="us-east-1"
export MANGO_FILE_S3_BUCKET="mango-files"
export MANGO_FILE_S3_ACCESS_KEY="mango-local"
export MANGO_FILE_S3_SECRET_KEY="mango-local-development-only"
export MANGO_FILE_S3_PATH_STYLE="true"
export MANGO_FILE_S3_CREATE_BUCKET="true"

# Vault and Webhook keyring (the Compose stack mounts its development-only keyring).
export MANGO_VAULT_KEYRING_FILE="$PWD/deployments/local/vault-keyring.json"
```

The `default` Temporal namespace is created automatically by `auto-setup`.

For commands run directly on the host, initialize the Mango schema before
creating Workspaces or API keys:

```sh
go run ./cmd/mango migrate
```

Only `MANGO_DATABASE_URL` is required. Normal commands refuse an uninitialized
or mismatched migration ledger; they never create the schema as a startup side
effect. Compose performs this step automatically, and its completed migration
container is expected to show `Exited (0)` in `docker compose ps -a`.
See the [development database baseline](../../docs/deployment.md#development-database-baseline)
when moving from the old migration chain; migration does not reset old data.

## Service conformance tests

Start only the infrastructure dependencies, then run every test that can be
executed without an external model or sandbox account:

```sh
docker compose -f deployments/local/compose.yaml up -d --wait postgres temporal nats seaweedfs
make test-service
```

This is the same suite run by CI. It covers real PostgreSQL migrations and
transactions, Temporal workflows and Activities, NATS reconciliation and
previews, the Files and Skills lifecycles through real SeaweedFS, the
HTTP-to-service vertical slice, and a Docker Environment-worker tool step. Each database test uses an
isolated schema; workflow, object, and worker cleanup is part of the assertions.

## Health checks

Each service declares a Docker `healthcheck`:

- **postgres** — `pg_isready -U postgres -d mango`
- **temporal** — `tctl --address temporal:7233 cluster health`
- **nats** — HTTP `GET /healthz` on the monitoring port
- **seaweedfs** — HTTP `GET /healthz` on S3 port `8333`
- **api** — HTTP `GET /readyz`
- **worker** — its long-running orchestration process is alive

`docker compose ps` shows `(healthy)` once each passes.

## Teardown

```sh
make local-down            # stop containers, keep data
make local-down VOLUMES=1  # also delete the Postgres and SeaweedFS volumes
```

## Scope

This stack is for local development and integration tests only. It already
keeps API and worker process roles separate, but it is not a production
deployment manifest: end-user authorization, TLS, secrets, rolling worker versioning,
managed persistence, observability, resource limits, and production object
storage remain deployment work. The bundled SeaweedFS credentials and deterministic
Vault keyring are not a production recommendation. Files startup reconciliation
also currently requires one Files-enabled API process. See
[the deployment model](../../docs/deployment.md).


### Development object store

The stack uses the official Apache-2.0 community image for
[SeaweedFS 4.48](https://github.com/seaweedfs/seaweedfs/releases/tag/4.48), pinned
by its multi-platform manifest digest in `compose.yaml`. No source build,
enterprise license, or vendor account is required. The single-process `mini`
server runs Master, Volume, Filer, and S3 components. Admin UI and WebDAV are
disabled, and only the S3 port is published. Volume allocation is bounded to
16 volumes with a 1 GiB growth limit per volume; data is not preallocated.
These are development sizing choices, not a production capacity guarantee.

The `seaweedfs-data` volume persists both object bytes and Filer metadata under
`/data`. Stop or recreate the container without deleting that volume to retain
its namespace and contents. A backup must preserve the metadata and bytes
consistently, as well as Mango's PostgreSQL metadata. Copying only SeaweedFS
volume data files is not a complete backup. The bundled single-node stack is
not a production HA or backup solution; production remains an operator-owned
S3-compatible service configured through Mango's existing settings.

### Moving from the old MinIO stack

SeaweedFS uses a new data volume and new development credentials. It cannot
read MinIO's on-disk format, and Mango does not automatically transfer objects.
Do not start the new stack against retained PostgreSQL File/Skill records and
an empty SeaweedFS bucket: those records would still point to the old objects.

Stop the old stack with `make local-down` from its original checkout first.
Keep its checkout and all data together if you need to return to it. For a
**disposable** development stack, run the following from the new checkout:

```sh
docker compose -f deployments/local/compose.yaml down --volumes --remove-orphans
make local-up
```

This resets the configured PostgreSQL/Temporal and SeaweedFS state and removes
old containers. The old `mango-local_minio-data` volume is not declared in the
new manifest and is retained; remove it separately only after deciding its data
is no longer needed. If retaining Mango records, transfer all objects through
S3 with their original bucket and keys before starting Mango on the new
endpoint, and verify the transfer. There is no bundled data-migration command.
