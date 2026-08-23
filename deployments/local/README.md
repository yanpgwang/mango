# Local development stack

This directory builds and runs the complete local control plane with pinned
infrastructure versions and health checks.

| Service    | Image                          | Ports                       | Purpose                                              |
| ---------- | ------------------------------ | --------------------------- | ---------------------------------------------------- |
| PostgreSQL | `postgres:17.5-alpine`         | `5432`                      | Application event ledger, projections, admission outbox (and Temporal's own persistence, on separate databases). |
| Temporal   | `temporalio/auto-setup:1.29.7` | `7233` (gRPC)               | Durable session/thread orchestration.                |
| Temporal UI| `temporalio/ui:2.52.1`         | `8233` → container `8080`   | Workflow explorer at <http://localhost:8233>.        |
| NATS Core  | `nats:2.11.17-alpine`          | `4222` (client), `8222` (monitoring) | Ephemeral previews and SSE wakeups; PostgreSQL cursor reads repair loss. |
| MinIO      | `minio/minio:RELEASE.2025-09-07T16-13-09Z` | `9000` | S3-compatible File bytes for development and service conformance. |
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

The API bootstraps `sk-mango-local-development` for the default Workspace.
Override it with `MANGO_API_KEY` before `make local-up`; never reuse the
bundled value outside local development.

`make health` returns only once Postgres accepts connections, the Temporal
frontend answers `cluster health`, NATS `/healthz` is green, MinIO answers its
live probe, the API answers `/readyz`, and the worker process is alive.

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
export MANGO_FILE_S3_ACCESS_KEY="minioadmin"
export MANGO_FILE_S3_SECRET_KEY="minioadmin"
export MANGO_FILE_S3_PATH_STYLE="true"
export MANGO_FILE_S3_CREATE_BUCKET="true"

# Vault API keyring (the Compose stack mounts its development-only keyring).
export MANGO_VAULT_KEYRING_FILE="$PWD/deployments/local/vault-keyring.json"
```

The `default` Temporal namespace is created automatically by `auto-setup`.

## Service conformance tests

Start only the infrastructure dependencies, then run every test that can be
executed without an external model or sandbox account:

```sh
docker compose -f deployments/local/compose.yaml up -d --wait postgres temporal nats minio
make test-service
```

This is the same suite run by CI. It covers real PostgreSQL migrations and
transactions, Temporal workflows and Activities, NATS reconciliation and
previews, the Files lifecycle through real MinIO, the HTTP-to-service vertical
slice, a Docker sandbox tool step, and an offline coding-agent scenario that
observes failing assertions, fixes a mounted fixture, and publishes the verified
source through Session Outputs. Each database test uses an isolated schema;
workflow, object, and sandbox cleanup is part of the assertions.

## Health checks

Each service declares a Docker `healthcheck`:

- **postgres** — `pg_isready -U postgres -d mango`
- **temporal** — `tctl --address temporal:7233 cluster health`
- **nats** — HTTP `GET /healthz` on the monitoring port
- **minio** — HTTP `GET /minio/health/live`
- **api** — HTTP `GET /readyz`
- **worker** — its long-running orchestration process is alive

`docker compose ps` shows `(healthy)` once each passes.

## Teardown

```sh
make local-down            # stop containers, keep data
make local-down VOLUMES=1  # also delete the Postgres and MinIO volumes
```

## Scope

This stack is for local development and integration tests only. It already
keeps API and worker process roles separate, but it is not a production
deployment manifest: end-user authorization, TLS, secrets, rolling worker versioning,
managed persistence, observability, resource limits, and production object
storage remain deployment work. The bundled MinIO credentials and deterministic
Vault keyring are not a production recommendation. Files startup reconciliation
also currently requires one Files-enabled API process. See
[the deployment model](../../docs/deployment.md).
