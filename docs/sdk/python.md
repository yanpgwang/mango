---
title: Python SDK
description: Typed synchronous and asynchronous clients for Mango.
---

# Python SDK

Use `mango_sdk` with Python 3.11+. It includes `Mango` and `AsyncMango` clients
and generated request/response `TypedDict` types. The distribution name is
`mango-sdk`; the Python import remains `mango_sdk`.

## Install

The resource-based API in this guide is the unreleased `0.1.0a2` source version.
Use [source installation](../sdk.md#install-from-source). Published alpha 1 uses
an earlier interface.

## Configure the client

::include[../../sdk/python/examples/quickstart.py#client]{lang="python"}

Use a context manager or call `client.close()` when finished. `AsyncMango` uses
`async with` and awaited operations. Keys authenticate to your Mango server;
model-provider credentials stay on the worker. Do not append `/v1` to `base_url`.

## Methods and inputs

Use resource methods such as `client.sessions.create`,
`client.sessions.events.send`, and `client.memory_stores.memories.create`.
Path IDs are positional in HTTP hierarchy order; request fields and query filters
are keyword arguments, without a `body` wrapper. `types[]` becomes `types` and
`created_at[gte]` becomes `created_at_gte`. Both clients expose the same tree.
Responses remain typed dictionaries, for example `agent["id"]`.

Omit an argument or pass `NOT_GIVEN` to leave it absent. `None` sends explicit
JSON null; nested dictionary keys also retain omission.
`False`, zero, empty strings, and empty lists are preserved. Types guide static
checking; server-side validation remains authoritative.

## Streaming and pagination

Entering `with client.sessions.events.stream(session_id)` establishes the
subscription before the next statement. Send input inside that context, then
iterate envelopes with `event` and decoded `data` fields. Exit closes the stream.
The async variant uses `async with` and `async for`.

`sessions.events.iter` follows pages; `sessions.events.list` fetches one page.
The stream is live-only; reconnect by opening a stream and reconciling persisted
history. The default read timeout for streams is unbounded; the quickstart
sets a 60-second read timeout so a stalled example fails visibly.

## Errors and retry safety

`APIError` exposes `status_code`, `type`, and `request_id`. Calls do not
automatically retry mutations. A timeout does not prove the server rejected a
message; check history before retrying.

- [Runnable multi-language quickstart](../getting-started.md)
- [Events and recovery](../api/events.md)
- [Complete SDK README and examples](https://github.com/yanpgwang/mango/tree/main/sdk/python)
