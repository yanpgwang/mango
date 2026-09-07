---
title: TypeScript and JavaScript SDK
description: Typed clients, promises, pagination, and live event streams.
---

# TypeScript and JavaScript SDK

Use `mango-sdk` from Node.js 22+ with ESM imports. JavaScript uses the
same package; TypeScript adds generated request/response types. The client uses
native `fetch` and has no runtime dependencies.

## Install

The resource-based API in this guide is the unreleased `0.1.0-alpha.2` source
version. Use [source installation](../sdk.md#install-from-source). Published
alpha 1 uses an earlier interface.

## Configure the client

::include[../../sdk/typescript/examples/quickstart.ts#client]{lang="ts"}

The constructor accepts your Mango server URL, never a hosted-agent URL. A
reverse-proxy prefix is supported; do not append `/v1` yourself. Keep the
Workspace API key in trusted server-side code, not a public browser bundle.

## Methods and inputs

Use resource methods such as `client.sessions.create`,
`client.sessions.events.send`, and `client.memoryStores.memories.create`.
Path IDs are positional in HTTP hierarchy order. The next argument contains
request fields and query filters directly; the final argument holds request
options such as an `AbortSignal`. Query names use `types`, `event_deltas` and
`created_at_gte`; the transport restores their HTTP bracket spelling.

Omit a property or use `undefined` to leave it absent. Explicit `null`, `false`,
zero, and empty collections retain their wire meanings.

## Streaming and pagination

`sessions.events.stream` resolves after the subscription is established. Subscribe
before sending input, then iterate and close the returned handle in `finally`.
`sessions.events.streamMessages` provides lazy raw SSE metadata when needed.
Streams do not replay past events or reconnect automatically.

`sessions.events.listItems` follows pages lazily; `sessions.events.listPages` exposes
the pages, and `sessions.events.list` makes one request. Filters are preserved.

## Errors and retry safety

`APIError` exposes `status`, `type`, and `requestId`. Cancellation uses an
`AbortSignal`. There are no automatic write retries: if a response is lost,
check persisted events before resending a message or tool result.

- [Runnable multi-language quickstart](../getting-started.md)
- [Events and recovery](../api/events.md)
- [Complete SDK README and examples](https://github.com/yanpgwang/mango/tree/main/sdk/typescript)
