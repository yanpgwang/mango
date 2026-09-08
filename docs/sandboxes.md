---
title: Sandboxes
description: The self-hosted execution boundary and Docker reference worker.
---

# Sandboxes

Mango has one execution mode: `self_hosted`. The control plane coordinates the
agent loop and leases Environment Work; an operator-run worker executes shell
and file tools in infrastructure chosen and trusted by that operator.

Mango does not operate a shared cloud sandbox fleet and the control plane does
not import provider SDKs. Docker is the OSS reference launcher, not a public
Environment type or a control-plane provider setting.

## Execution boundary

```text
Mango control plane
  Environment Work + Session events + correlated results
                         |
Mango SDK worker protocol
  Poll -> Ack -> Heartbeat -> execute -> Send result -> Stop
                         |
Operator launcher
  Docker today; another isolated compute service may be added later
```

The worker owns container credentials, filesystem preparation, workspace
retention, egress policy, and cleanup. Mango owns durable Session state,
permission decisions, Work lease fencing, result correlation, retries, and
recovery.

The first-party Docker launcher creates a named workspace volume per Session
and runs each activation in a container. The control-plane API and Temporal
worker do not mount the Docker socket.

## Tool ownership

| Capability | Owner |
| --- | --- |
| `bash`, `read`, `write`, `edit`, `glob`, `grep` | Environment worker |
| Skill bundle and Memory Store preparation | Environment worker through scoped Session APIs |
| Web Search and Web Fetch | Supporting model endpoint |
| Remote MCP | Mango orchestration worker |
| Work lease, approvals, event history, result journal | Mango control plane |

File API objects and Git repository declarations are not automatically mounted
in a self-hosted workspace. Stage them through the operator's launcher or clone
them from inside the sandbox under the operator's network and credential policy.
Memory Stores are the supported Session Resource type; immutable Skill pins are
also prepared by the worker.

## Why provider launchers stay outside the control plane

The public CMA self-hosted cookbook demonstrates Docker and several commercial
compute platforms, but the protocol remains the same: operator code launches
compute and runs the generic worker there. Mango follows that separation. A
future Daytona, Modal, Cloudflare, Vercel, Kubernetes, or sandbox-service example
should adapt the launcher, not add a provider enum or provider credentials to
Mango's API.

This keeps the self-hosted trust boundary honest and leaves room to add a Mango-
hosted product later as a separate backend without preserving the removed
pre-release registry.

## Run the Docker reference

Follow [Self-hosted worker](guides/self-hosted-worker.md). The supervisor uses a
Workspace API key plus an Environment ID to poll Work. Each claimed item yields
a short-lived, Session-scoped credential used inside the container; the
Workspace key is not passed into the Session container.

For lifecycle and recovery details, see
[Self-hosted workers](architecture/self-hosted-workers.md) and
[Environment Work](api/environment-work.md).
