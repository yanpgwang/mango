---
title: Python SDK
description: Typed synchronous and asynchronous clients for Mango.
---

# Python SDK

Use `mango_sdk` with Python 3.11+. It includes `Mango` and `AsyncMango` clients
and generated request/response `TypedDict` types. See [local installation](../sdk.md#install-from-source).

## Configure the client

::include[../../sdk/python/examples/quickstart.py#client]{lang="python"}

Use a context manager or call `client.close()` when finished. `AsyncMango` uses
`async with` and awaited operations. Keys authenticate to your Mango server;
model-provider credentials stay on the worker. Do not append `/v1` to `base_url`.

## Methods and inputs

Operation IDs become snake_case: `create_session`, `send_session_events`, and
`create_memory`. Path identifiers are positional; `body` and query values are
keyword arguments. For example, `types[]` becomes `types` and
`created_at[gte]` becomes `created_at_gte`.

Omit a dictionary key to omit a field; `None` sends explicit JSON null.
`False`, zero, empty strings, and empty lists are preserved. Types guide static
checking; server-side validation remains authoritative.

## Streaming and pagination

Entering `with client.stream_session_events(session_id)` establishes the
subscription before the next statement. Send input inside that context, then
iterate envelopes with `event` and decoded `data` fields. Exit closes the stream.
The async variant uses `async with` and `async for`.

`iter_session_events` follows pages; `list_session_events` fetches one page.
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
