---
title: Model configuration
description: Connect Mango to a model endpoint for open-ended agent work.
---

# Model configuration

Mango's orchestration worker sends inference requests to your configured model
endpoint. The current adapter requires a Messages-shaped `POST /v1/messages`
API with streaming. An OpenAI-shaped endpoint alone is not sufficient.

Complete the [Quickstart](../getting-started.md) first. You need a reachable
endpoint, a credential it accepts, and a model ID it serves. Real model calls
may incur charges.

## Create a local configuration

From the repository root:

```bash
make dev-env-init
```

Open `~/.config/mango/dev.env` in your editor and set:

```ini
MANGO_API_KEY=sk-mango-local-development
MANGO_MODEL_BASE_URL=https://api.example.com
MANGO_MODEL_API_KEY=replace-me
MANGO_MODEL_ID=your-model-id
MANGO_MODEL_AUTH=x-api-key
```

Use the endpoint's base URL without `/v1/messages`. `MANGO_MODEL_AUTH` accepts
`x-api-key` or `authorization-bearer`, depending on your endpoint.

The file uses literal `NAME=VALUE` lines, without shell quotes or `export`.
It lives outside the repository and must have no group or other permissions.
`make dev-env-init` creates it with mode `600` and leaves an existing file intact.

`MANGO_API_KEY` authenticates applications to Mango. `MANGO_MODEL_API_KEY`
authenticates the orchestration worker to the model endpoint. Keep the latter
out of client applications and sandbox containers.

## Apply the configuration

```bash
make local-up
make local-health
```

`make local-up` loads the configuration file and rebuilds/recreates the local
services as needed. This differs from the explicit offline command in the
Quickstart. The API remains at `http://localhost:8080`.

Set the Agent's `model` to an ID served by this endpoint. The quickstart programs
use `offline-fake`; use a real-model example below for your first model-backed
Session. Existing Sessions retain their Agent snapshot, so updating an Agent
alone does not change a Session you already created.

## Try an application

Choose an example and follow its setup and run commands:

- [Human-in-the-loop gate](../examples/hitl-gate.md): let the model request an
  application action or human decision, then continue from the result.
- [Specialist team](../examples/multi-agent-team.md): delegate a review and
  follow up with an existing specialist.
- [Multi-agent SDK guide](multi-agent.md#use-a-first-party-sdk): use the same
  resource hierarchy from Go, Python, or TypeScript.

The example wrapper loads your local configuration:

```bash
scripts/with-dev-env make demo-hitl-gate
```

This command starts an interactive client, not the Mango services. The wrapper
can use a different file through `MANGO_ENV_FILE`. See each example for its
client runtime requirements and cleanup.

## Enable tools

A text-only Agent needs no Environment worker. For `bash`, `read`, `write`,
`edit`, `glob`, or `grep` in the default `self_hosted` Environment, start a
[Docker worker](self-hosted-worker.md).

Web Search and Web Fetch execute at a supporting model endpoint and require
`always_allow`. Remote MCP tools execute through Mango's orchestration runtime.
Neither is redirected to the Docker worker. See [where tools run](../concepts.md#where-tools-run).

For self-hosted Sessions, stage ordinary files and repositories in the
operator-owned worker workspace. Memory Stores and pinned Skills are prepared
through the worker protocol.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Agent creation succeeds, but inference fails | Verify the endpoint implements streaming `/v1/messages`, accepts the selected auth mode, and serves the Agent's model ID. |
| Requests still use offline mode | Check the file loaded by `make local-up`; confirm it contains all three model endpoint/key/ID values. |
| A Session waits for a tool result | Start a worker for its Environment, or handle its application-owned action. See [Events](../api/events.md#handle-required-client-actions). |
| Web tools are rejected | Disable them if your model endpoint does not implement them; enabled Web tools must use `always_allow`. |

Keep orchestration workers on the same Temporal Task Queue consistently
configured. For process configuration, see [Deployment](../deployment.md).
Contributor-only endpoint and integration checks are listed in
[CONTRIBUTING.md](https://github.com/yanpgwang/mango/blob/main/CONTRIBUTING.md).
