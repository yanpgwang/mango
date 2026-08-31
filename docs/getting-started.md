---
title: Getting started
slug: /getting-started
sidebar_position: 2
---

# Getting started

This guide runs the server with its deterministic offline model and sends one
message through the full environment → agent → session → event flow.

## Requirements

- Docker with Compose
- Go 1.24+, Python 3.11+, or Node.js 22+ for the selected SDK
- `curl` for the readiness check; `jq` for the HTTP-only example

## Run the server

Run from the repository root. This command explicitly selects the offline model
and does not load `~/.config/mango/dev.env`:

```bash
export MANGO_API_KEY="${MANGO_API_KEY:-sk-mango-local-development}"
MANGO_MODEL_BASE_URL= MANGO_MODEL_API_KEY= MANGO_MODEL_ID= \
  docker compose -f deployments/local/compose.yaml up -d --build
make local-health
```

This builds and starts separate API and worker containers plus PostgreSQL,
Temporal, Temporal UI, NATS, and MinIO. The examples use `http://localhost:8080`;
the Workflow explorer is at `http://localhost:8233`. No model credentials are
required for this offline walkthrough.

The convenience command `make local-up` behaves differently: it automatically
loads an existing development environment file and can enable a real model.
Do not use it as an offline-only guarantee. Both startup paths use the Docker
sandbox provider: each Session that needs sandbox tools gets its own container.
The worker needs access to the local Docker daemon and its resource directory;
see [Docker worker configuration](deployment.md#docker-worker-configuration).

In another shell, verify readiness:

```bash
curl -i http://localhost:8080/readyz
```

The Compose API bootstraps the Workspace key selected above. In another shell,
export the same key for the protected examples below. If you did not override
it, the development-only value is:

```bash
export MANGO_API_KEY=sk-mango-local-development
```

## Choose a client

The examples below show the same workflow in TypeScript, Python, Go, and HTTP.
The selected language is shared across code groups. Python and TypeScript have
[published alpha packages](sdk.md#install-an-alpha); Go currently uses source
installation. These SDKs do not yet have a stable API contract. The HTTP variant
needs `curl` and `jq`.

The complete repository examples below install/build the SDK from this checkout
so it matches the server source. Run them from the repository root. For your own
application, use the published alpha or [install from source](sdk.md#install-from-source).

```sh tab="TypeScript" tab-group="mango-language"
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run build
node --experimental-strip-types sdk/typescript/examples/quickstart.ts
```

```sh tab="Python" tab-group="mango-language"
python3 -m venv .venv
.venv/bin/python -m pip install ./sdk/python
.venv/bin/python sdk/python/examples/quickstart.py
```

```sh tab="Go" tab-group="mango-language"
(cd sdk/go && go run ./examples/quickstart)
```

```sh tab="HTTP" tab-group="mango-language"
bash examples/sdk-quickstart.sh
```

Each complete example creates its own resources and cleans them up on exit.
The following sections explain excerpts from those exact executable files; they
share variables and are not separate standalone programs. In Go, the excerpts
run inside a function returning `error`; the full file includes its imports.

### Configure the client

`MANGO_BASE_URL` defaults to `http://localhost:8080`. Do not append `/v1`.
The Workspace key authenticates to Mango, not to the model provider.

::include[../sdk/typescript/examples/quickstart.ts#client]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#client]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#client]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#client]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Create an environment

A cloud Environment routes sandbox tools to the Mango worker. A
`self_hosted` Environment instead parks built-in calls for a client's
`user.tool_result`; it is not required for ordinary self-hosted Mango deployment.

::include[../sdk/typescript/examples/quickstart.ts#environment]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#environment]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#environment]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#environment]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Create an agent

The offline stack uses `offline-fake`. Agents are versioned; each Session keeps
the resolved definition captured at creation.

::include[../sdk/typescript/examples/quickstart.ts#agent]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#agent]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#agent]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#agent]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Create a session

Creating the Session without initial events does not start a model turn.

::include[../sdk/typescript/examples/quickstart.ts#session]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#session]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#session]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#session]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Send a message and observe the turn

Sending an event admits durable work; the response contains accepted input,
not the eventual agent reply. The SDK variants subscribe **before** sending,
then wait for `session.status_idle` with `stop_reason.type = end_turn`.
The HTTP-only variant polls persisted history for this fresh Session's first turn.

::include[../sdk/typescript/examples/quickstart.ts#stream]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#stream]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#stream]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#stream]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

The examples fail if the stream ends early or the turn needs attention.
They do not blindly retry a message after an ambiguous network failure. For an
existing or reconnected Session, [open a stream and reconcile history](api/events.md#stream-events)
before deciding whether to send again. Preview deltas are ephemeral, not durable output.

## Read persisted history

SDK iterators follow pagination. With raw HTTP, follow `next_page` until it is
null; the first-turn example is small enough for one page.

::include[../sdk/typescript/examples/quickstart.ts#history]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#history]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#history]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#history]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

To inspect live events with raw HTTP, open this in a separate terminal **before**
sending the next message; substitute the ID of a Session you have kept alive:

```sh
curl -N -H "Authorization: Bearer $MANGO_API_KEY" \
  "http://localhost:8080/v1/sessions/$SESSION_ID/events/stream"
```

## Use a real model endpoint

The same Compose stack supports real-model tasks with Docker sandboxes and
Files. You do not need to replace its API or worker with source processes.

Create the repository-external configuration file, then edit it:

```bash
make dev-env-init
$EDITOR ~/.config/mango/dev.env
```

Set the following values in that file. It uses literal `NAME=VALUE` lines,
without shell quotes or `export`; replace the model placeholders securely.
The stack already configures PostgreSQL, Temporal, NATS, and MinIO internally:

```ini
MANGO_API_KEY=sk-mango-local-development
MANGO_SANDBOX=docker
MANGO_SANDBOX_IMAGE=python:3.12-alpine
MANGO_MODEL_BASE_URL=https://api.example.com
MANGO_MODEL_API_KEY=replace-me
MANGO_MODEL_ID=your-model-id
MANGO_MODEL_AUTH=x-api-key
```

Use `authorization-bearer` for `MANGO_MODEL_AUTH` if your endpoint requires it.
Then apply that configuration:

```bash
make local-up
make local-health
```

The API and worker select Docker consistently. The default sandbox image
contains Python; it is pulled on first use if missing. Packages requiring other
language runtimes need an operator-selected image that includes those runtimes.
Model credentials are supplied only to the worker, not the API or Session
containers. Real model calls may incur charges.

Native `serve` and `orchestrate` processes also default to Docker. They must
agree on the provider and object-store configuration when run outside Compose;
see [Deployment model](deployment.md). The runtime choices are `docker`, `e2b`,
`cube`, `opensandbox`, and `daytona`; `local` and unknown values are rejected.
An unreachable Docker daemon fails worker startup instead of permitting host
execution. Remote-provider variables and live-test commands are listed in
[Sandbox backends](sandboxes.md).

The current built-in external-model adapter expects a Messages-shaped
`/v1/messages` API. This adapter constraint does not define Mango's public API
or permanently limit future model integrations.
Do not run workers with different model or sandbox configuration on the same
Temporal Task Queue. Keep credentials in the environment and never commit them.

Only this model endpoint is called; Mango does not call a separate hosted agent
service. Whether a credential is usable depends on whether its gateway permits
authenticated `POST /v1/messages` requests with streaming. The following
explicit live checks and examples exercise that endpoint:

```bash
# Checks the external Messages endpoint only. This makes a real, potentially
# billable request.
scripts/with-dev-env make test-model-live

# With the local PostgreSQL and Temporal services running, checks one complete
# durable platform turn against the same model endpoint.
scripts/with-dev-env make test-platform-live

# Runs the longer File Resource -> coding loop -> Session Output scenario.
# The wrapper loads ~/.config/mango/dev.env without copying secrets into the
# repository or evaluating their contents as shell syntax.
scripts/with-dev-env make test-coding-agent-live

# Runs the public-HTTP expense gate example. The real model generates the
# decide/escalate calls and the terminal prompts for the human decision.
scripts/with-dev-env make demo-hitl-gate

# Runs a real coordinator, two specialist Agents, one Advisor consultation,
# and an interactive follow-up on a persistent child Thread.
scripts/with-dev-env make demo-multi-agent-team
```

The [coding-agent iteration example](examples/coding-agent-iterate.md) is a
standalone Python SDK application that connects to a running Mango deployment.
It owns its inputs and is independent of the system-test commands above. The
[HITL gate example](examples/hitl-gate.md) documents the interactive public-HTTP
example and its application-owned action boundary. The
[specialist-team example](examples/multi-agent-team.md) verifies real-model
delegation, Advisor usage, and persistent Thread follow-up.

These commands never print the API key. The model-only smoke test does not
enable tools; the platform tests use an isolated Docker sandbox, the coding
scenario excludes Web Search/Fetch from its least-privilege toolset, and the
interactive examples remove provider credentials from their client processes.
Live checks
are excluded from public CI because external credentials, availability,
latency, user input, and cost are not deterministic.
Use a newly issued key if a credential has ever appeared in chat, logs, or shell
history.

Docker containers share the host kernel. The development stack is not a hardened
boundary for hostile multi-tenant workloads. The former local-provider unsafe
override is no longer supported. Before upgrading an older local-backed stack,
finish and delete its Sessions using the old worker; existing local workspaces
are not migrated or silently replaced by empty Docker containers.

## Reusable local credentials

Development secrets should not be committed or copied between worktrees.
Create a user-local file once, then edit the values you actually use:

```bash
make dev-env-init
$EDITOR ~/.config/mango/dev.env
```

Run a command with that environment explicitly:

```bash
scripts/with-dev-env make demo-coding-agent
```

The wrapper requires the file to have no group or other permissions. Set
`MANGO_ENV_FILE` only when a different repository-external path is needed.
It loads configuration; it does not start or reconfigure a deployment. The
example above also requires its [Python SDK setup](examples/coding-agent-iterate.md#run-the-example).

## Clean up

```bash
make local-down
```

Stop any source API and worker processes with Ctrl-C before running this command.
It keeps the
PostgreSQL and MinIO volumes. Add `VOLUMES=1` only when you intentionally
want to delete local data. PostgreSQL schema changes are applied by embedded,
versioned `goose` migrations when API or worker processes start.
