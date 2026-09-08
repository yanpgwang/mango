---
title: Runtime and sandbox
description: How durable orchestration is separated from operator-owned tool execution.
---

# Runtime and sandbox

Mango's Temporal worker owns the durable agent loop. It loads committed
transcript state, calls the configured model endpoint, records model and tool
boundaries, waits for approvals or external results, and commits the turn. It
does not execute shell or file tools and does not provision Session sandboxes.

## One provider-neutral boundary

For a `self_hosted` Environment, Mango persists Environment Work whenever a
Session needs an external tool result. A worker:

1. polls and acknowledges a Work item;
2. switches to the item's Session-scoped credential;
3. heartbeats the lease while recovering Session events;
4. executes enabled tools in its operator-owned sandbox;
5. submits a result correlated to the committed tool-use event; and
6. stops the Work item.

Lease loss fences the previous worker. Re-delivery and process restart are
normal recovery paths; in-memory ownership is never authoritative.

## Docker reference launcher

The OSS Docker launcher supervises Work with the Environment credential and
starts an item runner in a container. A named volume preserves `/workspace`
for the Session across activations. The Session token crosses the supervisor
boundary through one-shot stdin and is not stored in container environment or
command metadata.

Docker is an implementation choice at the worker edge. There is no
`MANGO_SANDBOX` control-plane setting, provider registry, or Docker socket in
the API/Temporal deployment.

## Other infrastructure

Another launcher can use a VM, commercial sandbox API, container platform, or
Kubernetes. It must preserve the same Work and Session protocol invariants; it
does not require a new Environment type. Provider credentials and lifecycle
remain operator-owned.

Mango may add a hosted sandbox backend in the future, but it would be an
explicit product and trust-boundary decision rather than a hidden variant of
self-hosted execution.

See [Self-hosted workers](self-hosted-workers.md) for the detailed protocol and
[Sandboxes](../sandboxes.md) for capability ownership.
