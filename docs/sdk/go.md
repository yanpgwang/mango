---
title: Go SDK
description: A typed, standard-library-only client for Mango.
---

# Go SDK

The standalone module `github.com/yanpgwang/mango/sdk/go` requires Go 1.24+
and has no server or third-party runtime dependencies. See [local installation](../sdk.md#install-from-source).

## Configure the client

The quickstart imports `context`, `os`, `time`, and the SDK as `mango`.
This excerpt runs inside a function returning `error`:

::include[../../sdk/go/examples/quickstart/main.go#client]{lang="go"}

Every operation accepts a context. Use a deadline or cancellation for long-lived
streams. `BaseURL` supports a reverse-proxy prefix; do not append `/v1`.

## Methods and inputs

Methods use exported OpenAPI operation IDs: `CreateSession`, `SendSessionEvents`,
and `CreateMemory`. Path identifiers are positional; requests and query filters
are generated structs.

- The zero value of `Optional[T]` omits a field.
- `mango.Some(value)` sends the value, including zero, `false`, or an empty list.
- `mango.SomePtr("text")` sets a nullable string.
- `mango.Null[T]()` sends explicit JSON null; the server checks whether it is allowed.
- Set exactly one pointer in a wire union. Unknown response variants remain in `Raw`.

## Streaming and pagination

`StreamSessionEvents` establishes the subscription before returning. Call it
before sending input, iterate with `Next()`, decode `Event()`, check `Err()`,
and close the stream. The stream is live-only and does not automatically reconnect.

`ListSessionEventsAutoPaging` follows pages; `ListSessionEvents` fetches one page.
Use the [open-stream-then-list recovery procedure](../api/events.md#stream-events)
to recover durable events after disconnects.

## Errors and retry safety

Use `errors.As` with `*mango.APIError` to access `StatusCode`, `Type`, and
`RequestID`. Avoid logging full error bodies, which may contain application data.
The SDK does not automatically retry writes after an ambiguous failure.

- [Runnable multi-language quickstart](../getting-started.md)
- [Complete SDK README and examples](https://github.com/yanpgwang/mango/tree/main/sdk/go)
