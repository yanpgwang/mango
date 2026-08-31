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

```bash
make local-up
make local-health
```

This builds and starts separate API and worker containers plus PostgreSQL,
Temporal, Temporal UI, and NATS. The examples use `http://localhost:8080`; the
Workflow explorer is at `http://localhost:8233`. No model credentials are
required because the worker defaults to the deterministic offline model.

In another shell, verify readiness:

```bash
curl -i http://localhost:8080/readyz
```

The Compose API bootstraps one development-only Workspace key. Export it once
for the protected examples below:

```bash
export MANGO_API_KEY=sk-mango-local-development
```

## Choose a client

The examples below show the same workflow in TypeScript, Python, Go, and HTTP.
The selected language is shared across code groups. Install your
[SDK from the local checkout](sdk.md#install-from-source) first; packages are not
published as stable registry releases. The HTTP variant needs `curl` and `jq`.

Run the complete example from the repository root:

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

The quick start already runs an offline worker. Stop it before launching a
source worker with different model or sandbox configuration:

```bash
docker compose -f deployments/local/compose.yaml stop worker

export MANGO_DATABASE_URL="postgres://postgres:postgres@localhost:5432/mango?sslmode=disable"
export MANGO_TEMPORAL_HOSTPORT="localhost:7233"
export MANGO_NATS_URL="nats://localhost:4222"

export MANGO_MODEL_BASE_URL=https://api.example.com
export MANGO_MODEL_API_KEY=replace-me
export MANGO_MODEL_ID=claude-model-id
export MANGO_MODEL_AUTH=x-api-key # or authorization-bearer

# A real model must not run against the local sandbox (it is a dev-grade
# guardrail, not a security boundary), so select the Docker sandbox for real
# isolation. The server refuses to start with a real model + local sandbox.
export MANGO_SANDBOX=docker

go run ./cmd/mango orchestrate
```

The provider name is validated strictly. The compiled choices are `local`,
`docker`, `e2b`, `cube`, `opensandbox`, and `daytona`; an unknown value fails
worker startup and never silently falls back to local host execution. Remote
provider variables and live-test commands are listed in
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
make test-model-live

# With the local PostgreSQL and Temporal services running, checks one complete
# durable platform turn against the same model endpoint.
make test-platform-live

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

The [coding-agent iteration example](examples/coding-agent-iterate.md) explains the
corresponding user workflow and the Mango resources involved. It is a design
walkthrough rather than a second test runner. The
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

If you understand the risk and deliberately want a real model against the local
sandbox during development, set `MANGO_ALLOW_UNSAFE_LOCAL_SANDBOX=1` to
override the startup guard. This is a dev-only escape hatch — the local sandbox
runs tool commands on the host with no isolation, so never use it with untrusted
input or in production.

## Reusable local credentials

Development secrets should not be committed or copied between worktrees.
Create a user-local file once, then edit the values you actually use:

```bash
make dev-env-init
$EDITOR ~/.config/mango/dev.env
```

Run a command with that environment explicitly:

```bash
scripts/with-dev-env go run ./cmd/mango orchestrate
```

The wrapper requires the file to have no group or other permissions. Set
`MANGO_ENV_FILE` only when a different repository-external path is needed.

## Clean up

```bash
make local-down
```

This keeps the PostgreSQL volume. Add `VOLUMES=1` only when you intentionally
want to delete local data. PostgreSQL schema changes are applied by embedded,
versioned `goose` migrations when API or worker processes start.
