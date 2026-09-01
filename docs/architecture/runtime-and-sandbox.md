---
title: Runtime and sandbox
---

# Runtime and sandbox

The runtime separates conversation orchestration, model inference, and tool
isolation.

## Agent runtime

`AgentRuntime.Run` receives one trigger, the immutable Session Agent snapshot,
the conversation projected from event history, parsed tool configuration, and
an acquired sandbox when tools require one.

The default `AgentCore`:

1. sends alternating `messages[]` to the model client;
2. streams assistant text as an optional preview;
3. emits the complete `agent.message`;
4. executes allowed built-ins and feeds results back to the model;
5. repeats until the model ends the turn or requires client action.

The loop is capped at 20 model/tool rounds.

## Model clients

The model boundary is a small stateless interface with regular and streaming
message creation. The offline fake is deterministic and requires no model
endpoint. The real client targets a Messages-shaped `/v1/messages` endpoint
with API-key or bearer authentication.

Conversation ownership remains in Mango. Provider-native history is retained
in a lossless transcript rather than reconstructed from public events. Native
Web Search/Fetch, MCP, and compaction keep separate raw, model-facing, and
public projections. See
[Storage, context, and connected tools](storage-context-and-tools.md).

## Tool runtime

Agent tool configuration resolves to the built-in toolset, custom tools, and
MCP toolset references. `bash`, `read`, `write`, `edit`, `glob`, and `grep`
execute in the Session sandbox. Provider-native Web Search/Fetch and remote MCP
cross their own network boundaries. Custom tools and interceptable
`always_ask` tools park the Session for a client response.

## Sandbox control plane

Mango keeps a narrow in-process lifecycle interface so Agent and Temporal code
do not depend on infrastructure SDK types. Its only implementation uses the
official OpenSandbox Go SDK. There is no runtime provider registry, host-process
executor, or direct Docker fallback.

```text
Mango Activity
  -> SessionManager
  -> Mango lifecycle/resource boundary
  -> OpenSandbox Go SDK
  -> OpenSandbox server
       -> Docker                  local development and CI
       -> Kubernetes + Kata       production-candidate profile
```

Mango owns durable Session bindings, recovery, resource reconciliation, Memory
versioning/writeback, and teardown. OpenSandbox owns workload creation,
attachment, command/file transport, volumes, network policy, and its runtime.
OpenSandbox endpoint, authentication, image, and proxy settings are worker
configuration and never appear in Mango's public Environment or Session wire
models.

Agent commands run as numeric UID/GID `1000:1000`; trusted package and resource
maintenance runs as sandbox root. The local OpenSandbox Docker profile enables
`no_new_privileges`. Only the OpenSandbox service mounts the Docker socket;
Mango API and worker processes stay non-root and never receive it.

Limited networking becomes an exact OpenSandbox deny-by-default host policy.
Provisioning temporarily expands it for configured package setup, restores the
final Environment policy before publishing the binding, and reconciles later
MCP-derived host changes.

The production-candidate path is:

```text
Mango/Temporal -> OpenSandbox -> Kubernetes BatchSandbox -> Kata RuntimeClass
```

Kata gives the sandbox a VM-backed kernel boundary rather than the local
Docker profile's shared host kernel. This is a deployment profile, not a new
public Environment type. It remains Preview until the
[promotion gates](../deployment.md#production-promotion-gates) pass against a
pinned release matrix. OpenSandbox-on-Docker conformance establishes adapter
behavior, not production isolation.

## Session-scoped ownership

A sandbox is scoped to a Session, not a Run. The first tool-using Run
idempotently creates it; later Runs reuse the same instance. Different Sessions
use different ownership keys and never share one logical sandbox.

PostgreSQL records a non-secret provisioning intent before OpenSandbox
creation, then atomically replaces it with provider name `opensandbox`, opaque
external ID, and spec hash after package/resource setup succeeds. The in-memory
map is only a live-client cache.

Entering idle retains the sandbox. After a worker restart, `Attach`
reconstructs the client from the persisted reference. A missing external
resource fails explicitly rather than silently replacing its workspace.

Session deletion fences admission, stops the Workflow, retries OpenSandbox
teardown, removes the binding, and only then deletes the Session row. Workers
also recover unfinished provisioning and deletion intents. All workers for a
deployment must reach the same OpenSandbox control plane and durable state.
Checkpoint/restore, quotas, and automatic eviction are not implemented.

## Streaming previews

The runtime may report `event_start` and `event_delta` through an optional
preview interface. These frames bypass durable storage and go directly to
subscribers that requested them. The complete event is still committed through
the normal transaction, so the event log remains authoritative and clients
must tolerate a preview ending without a persisted event after a failure.
