---
title: Mango
description: A self-hosted runtime for durable AI agents.
slug: /
sidebar_label: Overview
sidebar_position: 1
---

# Mango

Mango is an independent, Apache-2.0-licensed agent runtime
written in Go. It persists server-owned sessions, delegates inference to a
configured model endpoint, executes tools in replaceable sandboxes, and exposes
its own HTTP API for durable agent work.

:::warning[Experimental project]

This is a pre-release project with no customers and no supported stable API.
`/v1` is the single development API namespace. Routes, fields, schemas, and
behavior may change there directly; Mango does not
preserve earlier development snapshots through `/v2` or compatibility layers,
and does not promise drop-in use with a hosted agent service or third-party
SDK. Check [capabilities and limits](capabilities.md) before depending on a
workflow. The local OpenSandbox Docker runtime shares the host kernel and is not
a hardened hostile multi-tenant boundary.

:::

## What it does

- Server-owned **Agents, Environments, Sessions, and Events** over a `/v1` HTTP
  API, with cursor pagination and SSE streaming.
- First-party **Go, Python, and TypeScript/JavaScript SDKs**, with a
  [multi-language quickstart](getting-started.md) and [language guides](sdk.md).
- A durable **model-and-tool loop**: multi-round inference, custom-tool and
  confirmation waits, single- and multi-Thread interrupts, and outcome
  evaluation.
- Tools run through **OpenSandbox** — Docker for local/CI and a Kubernetes/Kata
  production-candidate profile — with eight built-ins plus provider-native Web
  Search/Fetch and remote MCP tools.
- Opt-in **live previews** of assistant text, streamed while the authoritative
  event is still being produced.

## How it fits together

The default deployment separates API and worker roles around three backends:

- **PostgreSQL** is authoritative for resources, public events, projections,
  admission, and the tool journal.
- **Temporal** durably runs each Session Workflow and replay-safe model and tool
  Activities.
- **NATS Core** carries best-effort previews and event wakeups; missed wakeups
  are repaired from PostgreSQL sequence cursors.

The [getting-started command](getting-started.md#run-the-server) explicitly selects
the deterministic offline model and needs no external model credentials;
`make local-up` can instead load a real model from an existing development
configuration. Examples use a documented development-only Mango Workspace key.
The runtime and its native API are the
product. External contracts may supply useful routes, resource models, schemas,
events, and public SDK types that Mango intentionally reuses or adapts. The
adopted result is Mango-owned: Mango's own documentation, OpenAPI definition,
implementation, and tests define current behavior and may evolve independently.

## Next steps

- **[Get started](getting-started.md)** — run the full stack and complete a
  first Session turn.
- **[SDKs](sdk.md)** — install a client and work in your preferred language.
- **[Product direction](product.md)** — what Mango optimizes for and how work is
  selected.
- **[Concepts](architecture.md)** — how the server owns history, and how
  sessions, events, and the runtime fit together.
- **[API reference](api/overview.md)** — implemented endpoints, request shapes,
  and transport conventions.
- **[Run a multi-agent Session](guides/multi-agent.md)** — configure a
  coordinator, delegate to persistent child Threads, and inspect their work.
- **[Capabilities and limits](capabilities.md)** — what is supported, limited,
  or still in preview.
