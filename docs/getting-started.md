---
title: Quickstart
description: Start Mango locally and complete a Session in your preferred language.
slug: /getting-started
---

# Quickstart

Run a text-only Session with the built-in offline model. You will create an
Environment and an Agent, send a message, and read its persisted reply. No
model-provider account or credentials are required.

## Requirements

- Git, Docker with Compose, and `make`.
- `curl` for the readiness check; `jq` if you choose the HTTP example.
- One SDK runtime if needed: Node.js 22+, Python 3.11+, or Go 1.24+.

## Get the code

```bash
git clone https://github.com/yanpgwang/mango.git
cd mango
```

Run the following commands from this directory.

## Run the server

```bash
export MANGO_API_KEY=sk-mango-local-development
MANGO_MODEL_BASE_URL= MANGO_MODEL_API_KEY= MANGO_MODEL_ID= \
  docker compose -f deployments/local/compose.yaml up -d --build
make local-health
```

Wait for every service to report `healthy`. The API listens on
`http://localhost:8080`; Temporal's workflow explorer is at
`http://localhost:8233`.

```bash
curl -i http://localhost:8080/readyz
```

Expect `HTTP/1.1 200 OK`. The stack starts Mango's API and orchestration worker,
PostgreSQL, Temporal, NATS, and MinIO. The model variables above explicitly
select offline mode. Later, use [Model configuration](guides/model-configuration.md)
to connect a real endpoint.

:::warning[Local development]

The example key and Compose configuration are for local use. Docker shares the
host kernel; this stack is not a hardened boundary for untrusted tenants. Use
[Deployment](deployment.md) to review its operating limits.

:::

## Run your first Session

Choose a language below. Each command runs a complete application that creates
its own resources and removes them when finished. Keep `MANGO_API_KEY` set in
this terminal; in a new terminal, export the same value again.

:::info[Install the SDK from this checkout]

These examples use the current resource-based SDKs, which are not yet published.
The commands below install them from source. For use in your own application,
see [SDK installation](sdk.md#install-from-source).

:::

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

The program prints the offline response and persisted history, then finishes with:

```text
Quickstart completed
```

The offline model exercises the Session lifecycle; it does not generate
open-ended answers or choose tools. A successful run confirms that your local
stack can accept input, complete a turn, and retrieve its recorded response.

## Understand the example

The following snippets come from the complete programs above. They share the
same client and resource variables; run the complete file to execute them
rather than pasting each excerpt as a separate program. Go excerpts belong
inside a function returning `error`.

### Configure the client

`MANGO_BASE_URL` defaults to `http://localhost:8080`. Do not append `/v1`.
The Workspace key authenticates to Mango, not to the model provider.

::include[../sdk/typescript/examples/quickstart.ts#client]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#client]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#client]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#client]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

### Create an environment

An Environment groups Sessions by their execution configuration. Omitting
`config` selects `self_hosted`. This example has no shell or file tools, so it
completes without a separate Environment worker.

::include[../sdk/typescript/examples/quickstart.ts#environment]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#environment]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#environment]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#environment]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

### Create an agent

The offline stack uses `offline-fake`. Agents are versioned; each Session keeps
the resolved definition captured at creation.

::include[../sdk/typescript/examples/quickstart.ts#agent]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#agent]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#agent]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#agent]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

### Create a session

Creating the Session without initial events does not start a model turn.

::include[../sdk/typescript/examples/quickstart.ts#session]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#session]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#session]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#session]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

### Send a message and observe the turn

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

### Read persisted history

SDK iterators follow pagination. With raw HTTP, follow `next_page` until it is
null; the first-turn example is small enough for one page.

::include[../sdk/typescript/examples/quickstart.ts#history]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#history]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../sdk/go/examples/quickstart/main.go#history]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../examples/sdk-quickstart.sh#history]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Clean up

The example deletes its Session and Environment and archives its Agent. Stop
the local services when you are done:

```bash
make local-down
```

This keeps the PostgreSQL and MinIO volumes for your next run. Use
`make local-down VOLUMES=1` only when you intend to delete the stack's stored data.

## Next steps

- [Connect a model](guides/model-configuration.md) for open-ended agent work.
- [Run a Docker worker](guides/self-hosted-worker.md) to enable shell and file tools.
- [Learn the core concepts](concepts.md) before adding resources or multi-agent work.
- [Explore the examples](examples/index.mdx) for approval gates and specialist teams.
