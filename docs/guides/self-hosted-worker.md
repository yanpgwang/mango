---
title: Docker worker
description: Run shell and file tools in containers on your infrastructure.
---

# Docker worker

The first-party Docker launcher connects an operator-run worker to a
`self_hosted` Environment. A trusted supervisor claims Work from Mango and
starts a container for each activation. The Session's workspace persists in a
named Docker volume between activations.

This worker is separate from `mango orchestrate`, which runs the model loop.
See [Environments and workers](../concepts.md#environments-and-workers) for the
process responsibilities.

## Prerequisites

- A running Mango deployment from the [Quickstart](../getting-started.md).
- Go 1.26.6+ to run the launcher from this repository.
- Docker reachable from the supervisor, plus `curl` and `jq` for setup.
- A [configured model](model-configuration.md) that can choose tools when you
  are ready to run a tool-capable Agent.

Run setup commands from the repository root. The model credential belongs to
the orchestration worker; the Docker supervisor needs only the Mango Workspace key.

## Create an Environment

In your setup terminal, select the Mango API and set the same Workspace key
used by that deployment. The following key is for the local development stack:

```bash
export MANGO_BASE_URL=http://localhost:8080
export MANGO_API_KEY=sk-mango-local-development

export MANGO_ENVIRONMENT_ID=$(curl -fsS "$MANGO_BASE_URL/v1/environments" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Docker worker"}' | jq -er .id)
```

Omitting `config` selects `self_hosted`. Keep this Environment ID: new Sessions
must use it to reach this worker. The Quickstart removes its own Environment,
so create this one independently.

## Build the worker image

```bash
docker build \
  -f deployments/self-hosted/docker/Dockerfile \
  -t mango-self-hosted-worker:local \
  .
```

This image supplies the worker process and its tool runtime. Build an image
with additional language runtimes or packages when your Agent needs them, and
select it with `MANGO_WORKER_IMAGE`.

## Start the supervisor

In the same setup terminal:

```bash
export MANGO_DOCKER_BASE_URL=http://host.docker.internal:8080
go run ./cmd/mango-worker docker
```

Keep this process running. It logs `starting Docker self-hosted worker` with
the Environment ID, then polls for activations. An empty queue is normal before
you send input to a Session.

`MANGO_BASE_URL` must be reachable from the host supervisor;
`MANGO_DOCKER_BASE_URL` must be reachable from its containers. The launcher adds
Docker's `host-gateway` mapping for `host.docker.internal`. For a remote API,
use its reachable URL; for a shared Docker network, use its DNS name and the
launcher's `--network` option.

## Use the worker in a Session

From your application or another terminal, create an Agent with the model ID
served by your deployment. This tool configuration enables only Bash:

```json
{
  "name": "Workspace assistant",
  "model": "your-model-id",
  "system": "Use Bash to inspect the workspace and explain your results.",
  "tools": [{
    "type": "agent_toolset_20260401",
    "default_config": {
      "enabled": false,
      "permission_policy": {"type": "always_allow"}
    },
    "configs": [{
      "name": "bash",
      "enabled": true,
      "permission_policy": {"type": "always_allow"}
    }]
  }]
}
```

Submit it with [Create an Agent](../api/agents.md#create-an-agent), then
[create a Session](../api/sessions.md#create) using that Agent ID and
the Environment ID from setup. Open its event stream before sending a request
such as `Use Bash to run pwd and report the working directory.`

A tool turn should produce an `agent.tool_use`, a worker-submitted result, and
an agent reply. The working directory is `/workspace`. If the Session waits
for a tool result indefinitely, check that the supervisor is running for the
same Environment and that its container can reach Mango. Use
[Environment Work](../api/environment-work.md) to inspect queued or active items.

## Filesystem and lifetime

| Location or resource | Behavior |
| --- | --- |
| `/workspace` | A named volume reused by later activations of the same Session. |
| Bash process | Preserves cwd, environment, and background jobs within one activation; a new activation starts a new shell. |
| Custom Skills | Downloaded from the Session's immutable pins and verified before tool execution. |
| `/mnt/memory` | Attached Memory Stores are prepared and synchronized; file tools enforce attachment access for writes and edits. |
| Work container | Removed when its activation exits. The workspace volume remains. |

Automatic File/Git preparation and Session output publication are not yet
implemented by this worker. Read-write Memory has its own durable synchronization;
ordinary workspace files stay in the Docker volume. Avoid assuming that a
workspace file is already a downloadable Mango File.

## Stop the worker

Press Ctrl-C in the supervisor terminal. SIGTERM/SIGINT cancels tools while
heartbeats continue through Memory teardown. The Docker reference gives each
container 120 seconds to finish result delivery, the bounded Memory sync/flush,
and Work Stop before a hard kill. The container carries that same default for
an ordinary `docker stop`; allow the supervisor at least 150 seconds if another
process manager controls its shutdown. A forced kill or failed/timed-out Memory
upload can still lose unsynchronized edits; uploaded Memory remains durable.

Inspect any active Session before
sending more work. Stopping the supervisor does not delete your Environment,
Session history, or retained workspace volumes. Manage Session deletion through
the API and remove a retained Docker volume only after its contents are no
longer needed.

The reference image runs without root or Linux capabilities, uses a read-only
root filesystem, and defaults to one CPU, 1 GiB memory, and 256 processes.
Docker still shares the host kernel and allows bridge egress by default. Set
network policy for your deployment; this preview is not a hardened hostile
multi-tenant boundary.

See the [launcher reference](https://github.com/yanpgwang/mango/tree/main/deployments/self-hosted/docker)
for credential handling and [Go SDK helpers](../sdk/go.md#composed-environment-worker)
for building your own isolated launcher.
