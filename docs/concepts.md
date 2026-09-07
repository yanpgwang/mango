---
title: Core concepts
description: Understand Agents, Sessions, Events, and the workers that execute tools.
---

# Core concepts

Mango separates an agent's definition, its ongoing work, and the infrastructure
that executes its tools. You can use the same Agent in many Sessions and observe
each Session independently through the API.

## Agents and Sessions

An **Agent** is a versioned definition: a model, instructions, tools, and optional
Skills or specialist roster. A **Session** is one persistent conversation and
execution lifecycle. It captures an Agent snapshot and selects an Environment.
Changing an Agent does not silently change existing Sessions.

A typical application:

1. Creates or selects an Agent and an Environment.
2. Creates a Session with their IDs.
3. Opens the Session event stream, then sends a message.
4. Observes replies or required actions and sends follow-up input as needed.
5. Archives the Session to retain its history, or deletes it when that history
   is no longer needed.

Creating a Session without initial events does not start a model turn. Sending
input accepts durable work; the HTTP response acknowledges that input, while
the eventual agent response arrives through Events.

## Events and turns

**Events** are the public history of a Session: user input, agent messages, tool
requests/results, usage, and lifecycle changes. Read recorded Events through
list endpoints and subscribe to new ones through SSE.

A **turn** processes input until it finishes, needs a client action, reaches a
limit, or fails. `session.status_idle` needs interpretation: `end_turn` means the
turn completed, while `requires_action` means an application or worker must
supply the requested result. Other stop reasons may require attention.

Streams are live-only. Subscribe before sending input. After a disconnect,
open a stream and reconcile persisted history, deduplicating by event ID.
Optional text previews are ephemeral and do not replace the event log. See
[Events and streaming](api/events.md#stream-events) for the recovery procedure.

## Environments and workers

An **Environment** selects the execution boundary for Sessions. Mango defaults
to `self_hosted`: you run a worker that claims Environment Work and executes
shell/file tools in infrastructure you control.

There are two different workers:

| Process | Responsibility |
| --- | --- |
| Orchestration worker (`mango orchestrate`) | Run the durable agent loop, call the model endpoint, and coordinate Session state. |
| Environment worker (`mango-worker docker`) | Claim Work for an Environment and execute shell/file tools inside Docker containers. |

The local Compose stack includes the orchestration worker. Start the
Environment worker separately when your Agent needs shell/file tools. A
text-only Session or an application-owned approval workflow can run without it.

**Environment Work** is a leased activation of a Session. The Docker supervisor
claims the activation using a Workspace API key; its container receives a
short-lived credential scoped to that Work. A later activation reuses the
Session's workspace volume. The shell process itself persists only within one
activation.

For setup, read [Docker worker](guides/self-hosted-worker.md) and consult
[capabilities](capabilities.md) for current limits.

## Where tools run

| Tool kind | Execution owner | What the application does |
| --- | --- | --- |
| Shell and file tools | Your Environment worker, in its sandbox | Run a worker for the Session's Environment. |
| Custom tools | Your application or service | Handle the requested action and submit its result. |
| Web Search / Web Fetch | A supporting model endpoint | Enable supported Web tools with `always_allow`. |
| Remote MCP tools | Mango's orchestration runtime, calling the MCP server | Configure the server, permissions, and optional Vault credentials. |

A tool's approval policy is separate from its execution owner. Approval gives
permission to run it; it is not the execution result. See
[required client actions](api/events.md#handle-required-client-actions).

## Threads and agent teams

Every Session has a primary **Thread**. A coordinator can delegate to persistent
child Threads, each with its own conversation, events, and usage. Threads share
the Session's budget and workspace. An **Advisor** is a separate consultation
available to the primary Agent; it is not a persistent child Thread.

Use the [multi-agent guide](guides/multi-agent.md) to configure a team, and the
[Thread API](api/session-threads.md) to inspect its work.

## Files, Skills, and Memory

- **Files** store immutable bytes for application and model-message workflows.
  A self-hosted launcher stages workspace files independently.
- **Skills** are versioned bundles of instructions and supporting files.
  Sessions pin immutable Skill Versions.
- **Memory Stores** keep versioned UTF-8 files across Sessions. The Docker worker
  prepares attached Stores and synchronizes changes according to their access mode.

These resources have different lifecycles; a file in a sandbox is not
necessarily a downloadable File object or a durable Memory entry. Read
[Session Resources](api/session-resources.md) for attachment rules.

## Workspaces and credentials

A **Workspace** is Mango's tenant boundary. Its API keys authenticate with
standard bearer authorization and share access to that Workspace's resources.
Your application owns end-user authorization. Model credentials stay with the
orchestration worker, and per-Work credentials limit sandbox access.

For the implementation and failure model, continue to [Architecture](architecture.md).
