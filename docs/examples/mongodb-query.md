---
title: Query MongoDB
description: Let an agent query your database from a self-hosted Docker worker.
slug: /examples/mongodb-query
---

# Query MongoDB

An inventory assistant uses Bash and Python's `pymongo` to find products that
need reordering. The database connection string lives in the worker container's
environment. The application sends a question to Mango and prints the agent's
answer.

The [example source](https://github.com/yanpgwang/mango/tree/main/examples/mongodb-query)
uses Mango's Go SDK for Agents, Sessions, Events, and Environment Work. Its own
Docker launch passes `MONGO_URI` into the container. This is application-owned
configuration; it needs no new Mango API field or change to the reference worker.

## Run with a local database

You need Go, Docker with Compose, and a running Mango deployment with a
[configured model](../guides/model-configuration.md). Use the same source revision
for the deployment and SDK. These commands run from the repository root.

Build the example worker and start its separate MongoDB container:

```bash
docker build -f examples/mongodb-query/Dockerfile -t mango-mongodb-example:local .
docker compose -f examples/mongodb-query/compose.yaml up -d --wait
```

The database contains three synthetic inventory records and exposes no host
port. The worker reaches it on the `mango-mongodb-example` Docker network.

Configure the application and run it:

```bash
export MANGO_BASE_URL=http://localhost:8080
export MANGO_DOCKER_BASE_URL=http://host.docker.internal:8080
export MANGO_API_KEY=your-mango-workspace-key
export MONGO_URI=mongodb://mongodb:27017
scripts/with-dev-env go run ./examples/mongodb-query
```

`scripts/with-dev-env` loads `MANGO_MODEL_ID` from your local development config.
Alternatively, set that variable yourself and run `go run ./examples/mongodb-query`.
The model provider's credentials belong to the Mango orchestration worker;
the example only needs the model ID and Mango Workspace key.

`MANGO_DOCKER_BASE_URL` must be reachable from the container. For a local Mango
API, bind it to an address Docker can reach, as described in the
[self-hosted worker guide](../guides/self-hosted-worker.md). On Linux the example
adds the standard `host.docker.internal:host-gateway` mapping.

The default question asks which products need reordering. You should see a Bash
tool call and an answer with:

- **TEA:** stock 4, reorder point 12; order 8.
- **MUG:** stock 2, reorder point 6; order 4.
- **COFFEE** needs no order: stock 18 is above its reorder point of 10.

The wording can vary. Inspect the tool output and answer as functional evidence; the
example is a small application, not a runtime conformance or recovery test.

Verified on 2026-09-13 with `claude-sonnet-4-5-20250929`, the local MongoDB above,
and Mango's self-hosted worker SDK. The Bash output and final answer both
identified MUG (4 units) and TEA (8 units). This verifies the inventory query
journey; it does not establish Atlas search or other database workflows.

## How the worker runs

The application creates a dedicated Environment and Session. The SDK's
`WorkPoller` polls and acknowledges one Work item; the application launches a
container and passes the scoped Work payload through stdin. Inside that
container, `EnvironmentWorker.HandleItem` owns heartbeats, tool execution,
result delivery, and Work Stop. The application keeps reading Events across
`requires_action` pauses until the agent ends its turn.

`MONGO_URI` is passed with Docker's `--env MONGO_URI`. Its value is absent from
the Agent definition and user message. The agent's shell can read it, as it
must to connect; use a database account with the permissions appropriate for
your task. The Workspace API key is not passed to the container.

## Use your own inventory collection

Set `MONGO_URI` to a MongoDB connection string reachable from the container.
The image includes SRV support for `mongodb+srv://` connections. Your collection
should have `sku`, `name`, `stock`, and `reorder_point` fields. The example queries
it and does not seed or remove records from your database.

| Variable | Default | Purpose |
| --- | --- | --- |
| `MONGO_DATABASE` | `mango_example` | Database to query. |
| `MONGO_COLLECTION` | `inventory` | Collection to query. |
| `MONGO_DOCKER_NETWORK` | `mango-mongodb-example` | Docker network; use `bridge` when the local demo network is unnecessary. |
| `MONGO_WORKER_IMAGE` | `mango-mongodb-example:local` | Image built above. |
| `MANGO_EXAMPLE_PROMPT` | Replenishment question | Ask another read-only inventory question. |

## Cleanup

The application removes its worker container, deletes its Session and
Environment, and archives its Agent on exit. Its workspace is temporary. It
leaves the database running so you can inspect or query it again.

Remove the example database and its disposable data when finished:

```bash
docker compose -f examples/mongodb-query/compose.yaml down --volumes
```

If the host application is forcibly killed, its printed Session ID and Docker
container name (`mango-mongodb-<work-id>`) identify resources to clean up manually.

The scenario follows the direct-database-access pattern in CMA's
[Docker self-hosted example](https://github.com/anthropics/claude-cookbooks/tree/main/managed_agents/self_hosted_sandboxes/docker).
Mango runs its own control plane and worker SDK. This example does not implement
the separate Atlas fraud-review, search, or human-approval cookbook.
