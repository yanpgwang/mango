---
title: TypeScript and JavaScript SDK
description: Typed clients, promises, pagination, and live event streams.
---

# TypeScript and JavaScript SDK

Use `mango-sdk` from Node.js 22+ with ESM imports. JavaScript uses the
same package; TypeScript adds generated request/response types. The client uses
native `fetch` and has no runtime dependencies.

## Install

Install the published alpha by its exact version:

```sh
npm install mango-sdk@0.1.0-alpha.1
```

This is an alpha, not a stable release, even if npm displays it under `latest`.
The package includes compiled JavaScript and type declarations. For development
against this checkout, see [source installation](../sdk.md#install-from-source).

## Configure the client

::include[../../sdk/typescript/examples/quickstart.ts#client]{lang="ts"}

The constructor accepts your Mango server URL, never a hosted-agent URL. A
reverse-proxy prefix is supported; do not append `/v1` yourself. Keep the
Workspace API key in trusted server-side code, not a public browser bundle.

## Methods and inputs

Method names follow OpenAPI operation IDs: `createSession`, `sendSessionEvents`,
and `createMemory`. The first argument contains path/query parameters and
`body`; the second contains request options such as an `AbortSignal`.
Omit a property or use `undefined` to leave it absent. Explicit `null`, `false`,
zero, and empty collections retain their wire meanings.

## Streaming and pagination

`openSessionEvents` resolves after the subscription is established. Subscribe
before sending input, then iterate and close the returned handle in `finally`.
The lower-level `streamSessionEvents` generator is lazy: construction alone
does not open a subscription. Neither API replays past events or reconnects.

`listSessionEventsItems` follows pages lazily; `listSessionEventsPages` exposes
the pages, and `listSessionEvents` makes one request. Filters are preserved.

## Errors and retry safety

`APIError` exposes `status`, `type`, and `requestId`. Cancellation uses an
`AbortSignal`. There are no automatic write retries: if a response is lost,
check persisted events before resending a message or tool result.

- [Runnable multi-language quickstart](../getting-started.md)
- [Events and recovery](../api/events.md)
- [Complete SDK README and examples](https://github.com/yanpgwang/mango/tree/main/sdk/typescript)
