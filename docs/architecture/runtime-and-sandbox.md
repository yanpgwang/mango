---
title: Runtime and sandbox
---

# Runtime and sandbox

The runtime is split into three replaceable boundaries: conversation
orchestration, model inference, and tool isolation.

## Agent runtime

`AgentRuntime.Run` receives a single trigger, the immutable session agent
snapshot, the conversation already projected from event history, the parsed
tool configuration, and a provisioned sandbox.

The default `AgentCore`:

1. sends alternating `messages[]` to the model client;
2. streams assistant text as an optional preview;
3. emits the complete `agent.message`;
4. executes allowed built-ins and feeds results back to the model;
5. repeats until the model ends the turn or requires client action.

The loop is capped at 20 model/tool rounds to prevent unbounded execution.

## Model clients

The model boundary is a small stateless interface with regular and streaming
message creation.

- The offline fake is deterministic and requires no model endpoint. The default
  worker still needs Docker; pulling a missing sandbox image requires access to
  its registry.
- The real client targets a Messages-shaped `/v1/messages` endpoint and
  supports API-key or bearer authentication.

Conversation ownership remains in this server. This is why earlier
self-contained harness integrations were removed: a second component owning
history would create competing sources of truth.

The context boundary now includes a lossless Provider Transcript rather than
reconstructing provider-native history from public events. Native Web
Search/Fetch and MCP keep separate raw, model-facing, and public projections;
compacted projections are preserved per Thread in immutable internal snapshots. See
[Storage, context, and connected tools](storage-context-and-tools.md).

## Tool runtime

Agent tool configuration is parsed into:

- the `agent_toolset_20260401` built-in toolset;
- custom tool definitions;
- MCP toolset references.

`bash`, `read`, `write`, `edit`, `glob`, and `grep` execute in the Session
sandbox. `web_fetch` and `web_search` are sent as native server-tool
declarations to the configured Messages API endpoint and currently require
`always_allow`. Remote MCP tools are discovered through Streamable HTTP, pinned
per Session, permission-checked, journaled, and given the same sandbox-backed
large-result policy. Deployment-managed MCP authentication and deprecated-SSE
fallback remain follow-up work.

Locally executable built-ins and MCP tools with `always_allow` execute inside
the current run. Custom tools and interceptable `always_ask` tools park the
session for a client response.

## Sandbox provider

The application provisions a sandbox only when the resolved toolset contains
tools. The same interface supports process execution and confined file reads
and writes. This is currently an in-process Go interface, not a separate
sandbox HTTP service. See the [sandbox backend matrix](../sandboxes.md) for
support levels, backend requirements, and the ordered evolution path.

### Docker provider

Docker is the default; `MANGO_SANDBOX=docker` selects it explicitly. The worker
requires a reachable Docker daemon even with the offline model. Host-process
execution is not selectable, and no unsafe-local override is supported. The
provider talks directly to the Docker Engine API through the supported Moby Go
client; it does not invoke the `docker` CLI. The client honors `DOCKER_HOST`,
`DOCKER_API_VERSION`, and Docker TLS environment variables and negotiates a
compatible API version by default. When an image is absent, the provider pulls
it through the Engine API and resolves registry credentials from the standard
Docker `config.json` selected by `DOCKER_CONFIG` or `~/.docker`. Inline `auths`,
`credsStore`, and per-registry `credHelpers` are supported; a configured native
credential helper must be installed on the worker. Containers use a separate
filesystem, Linux namespaces/cgroups, configurable resource limits, and no
networking for direct provider calls. Mango's cloud Environment path explicitly
requests `bridge` networking because the public Environment default is
unrestricted. The default image is `python:3.12-alpine`; operators select another
image with `MANGO_SANDBOX_IMAGE`. The Compose worker mounts its resource directory
at the same absolute path seen by the daemon. See
[Docker worker configuration](../deployment.md#docker-worker-configuration).

Containers share the host kernel. This provider has not been audited for
hostile multi-tenant use; stronger isolation such as gVisor or a remote sandbox
can be added behind the same provider interface.

### Remote providers

E2B, CubeSandbox, OpenSandbox, and Daytona are selected through the same
deployment-level registry. The worker remains the lifecycle owner: it maps one
session to one external sandbox, persists only the provider name and opaque ID,
reattaches after restart, and deletes the resource through a durable Temporal
Activity. The external service owns isolation and workspace storage.

Provider credentials, endpoint routing, templates, images, and auto-pause
settings stay in worker environment variables. They do not change the Managed
Agents Environment or Session wire models.

OpenSandbox is the first adapter with fine-grained egress capability. A
limited cloud Environment becomes a deny-by-default OpenSandbox network policy
whose exact allow rules are reconciled during provisioning, after an
MCP-affecting Session update, and after worker restart. Package installation
uses a temporary registry expansion before the durable binding is published;
tool execution sees only the final policy. The API returns `422` for limited
networking when any other implemented adapter is selected.

## Session-scoped ownership

A sandbox is scoped to the session, not to a single run. The first run in a
session that needs tools provisions a logical sandbox; every later run in the
same session reuses that same instance, so filesystem state a tool creates in
one run is visible to the next. Different sessions acquire under different keys
and never share a sandbox, so they stay isolated even when they use the same
agent and environment.

Ownership lives in a session-scoped manager that wraps the provider inside the
`internal/sandbox` package. PostgreSQL stores a non-secret provisioning intent
before provider creation, then atomically replaces it with the provider name,
opaque external ID, and spec hash. The in-memory map is only a live client
cache. The `AgentRuntime` is unaware of this — the application resolves the
sandbox and passes it in the run request.

Entering idle does not tear the sandbox down. After a worker restart, `Attach`
reconstructs the client from the persisted reference; if the resource has
disappeared, acquisition fails instead of silently replacing session state.

Session deletion runs provider teardown as a retryable Temporal Activity and
removes the binding before PostgreSQL permits the Session row to be deleted.
Workers reconcile provisioning intents left before binding and resume fenced
deletions left before cleanup or finalization. Local references require the same
host filesystem and Docker references require the same daemon. Remote
multi-worker execution therefore needs a service-backed provider.
Every provider name still present in a binding or provisioning intent must stay
routable to a compatible worker; changing the default provider does not migrate
existing resources or discharge their cleanup obligations.
Checkpoint/restore, quotas, and eviction are not implemented.

## Streaming previews

The runtime may report `event_start` and `event_delta` through an optional
preview interface. These frames bypass durable storage and go directly to
subscribers that requested the matching event type. The complete event is still
buffered and committed through the normal completion transaction.

This split keeps the event log authoritative while improving latency, but it
also means clients must tolerate a preview ending without a persisted event if
the process or upstream stream fails.
