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

## Self-hosted Work polling

`NewWorkPoller` claims and acknowledges Work for one `self_hosted` Environment.
It is a control-plane iterator, not a sandbox or tool runner. Advancing or
closing it never stops Work. `EnvironmentWorker` composes it with the Session
runner, lease heartbeat, and final Stop.

```go
poller := mango.NewWorkPoller(ctx, client, mango.WorkPollerOptions{
    EnvironmentID: environmentID,
    Drain:         true,
})
defer poller.Close()

for poller.Next() {
    work := poller.Current()
    // Launch a per-Session runner for work.Data.ID.
}
if err := poller.Err(); err != nil {
    return err
}
```

Drain mode omits `block_ms` unless explicitly configured and returns normally
when the queue is empty. A long-running poller defaults to the API's 999 ms
long-poll and exits normally when its context is cancelled. If Ack has an
ambiguous failure, the poller exits without calling Stop; Mango's Work TTL then
makes the activation reclaimable.

## Composed Environment worker

Use `NewEnvironmentWorker` for the complete provider-neutral Work lifecycle:

```go
worker := mango.NewEnvironmentWorker(client, mango.EnvironmentWorkerOptions{
    EnvironmentID: environmentID,
    Tools:         tools,
})
if err := worker.Run(ctx); err != nil {
    return err
}
```

The trusted supervisor client is used only for Poll and Ack. Before executing
a tool, the worker decodes the acknowledged Work secret, switches to its scoped
Session token, and obtains a successful conditional heartbeat. That token then
authorizes heartbeats, Session events, and final Stop. Missing or malformed
secrets fail closed; there is no Workspace-key fallback.

For a launcher that Polls and Acks outside a Session sandbox, call
`HandleItem` inside the sandbox with `EnvironmentWorkerHandleItemOptions`.
Non-secret identity fields may be passed explicitly or through `MANGO_WORK_ID`,
`MANGO_ENVIRONMENT_ID`, and `MANGO_SESSION_ID`. Pass `WorkSecret` explicitly
from a protected launcher transport when the process runs untrusted code. The
item process needs the Mango base URL but must not receive the Workspace key.
An allowlisted child environment prevents ordinary inheritance, but on Linux
the launcher must also keep secrets out of parent environment/command metadata
and prevent same-identity children from inspecting the trusted runner. The
first-party Docker launcher uses one-shot stdin and a non-dumpable item process.

The worker does not create compute or prepare Files, Git repositories, Skills,
or Memory. Those are launcher responsibilities. On lease loss it cancels the
runner, prevents later result submission, returns
`ErrEnvironmentWorkLeaseLost`, and does not Stop a possibly newer owner's Work.

For launchers that need Mango's core shell and file executors, the independent
`tools/agenttoolset` package returns `bash`, `read`, `write`, `edit`, `glob`, and
`grep` as `SessionTool` implementations:

```go
tools, err := agenttoolset.New(agenttoolset.Context{Workdir: "/workspace"})
if err != nil {
    return err
}
defer agenttoolset.CloseAll(tools)
```

This package is not a sandbox. The caller must establish the isolation
boundary first. File operations remain beneath `Workdir`, reads and outputs are
bounded, and Mango credentials are removed from the Bash environment. Bash uses
one persistent PTY session per toolset, so cwd, exported variables, and
background jobs survive calls. Its input accepts `command`, `restart`, and
`timeout_ms`; the runner-wide tool deadline remains the hard upper bound. A
timeout or cancellation replaces the shell before the next call. Bash is not
path-confined inside the host process, so the caller's sandbox is the security
boundary. The caller owns `CloseAll` because `SessionToolRunner` borrows tools
and never closes them.

## Self-hosted Session tools

`NewSessionToolRunner` handles one Session's provider-neutral tool loop. It
opens SSE before listing durable history, reconciles again on reconnect, and
maps owned `agent.tool_use` and `agent.custom_tool_use` events to their matching
result events. MCP calls stay server-side, and unregistered tool names remain
pending for another owner.

```go
runner := mango.NewSessionToolRunner(ctx, sessionClient, sessionID,
    mango.SessionToolRunnerOptions{Tools: tools})
defer runner.Close()

for runner.Next() {
    dispatch := runner.Current()
    // Observe dispatch.Owned, dispatch.IsError, and dispatch.Posted.
}
if err := runner.Err(); err != nil &&
    !errors.Is(err, mango.ErrSessionTerminated) &&
    !errors.Is(err, mango.ErrIdleTimeout) {
    return err
}
```

`sessionClient` should use the acknowledged Work item's `sessions_token`.
The runner fails closed on approval-gated or unknown permission values, and
returns `ErrSessionLeaseLost` when that credential loses authority. It does not
heartbeat or Stop Work; use `EnvironmentWorker` for that composition. Tools
must honor cancellation and use the stable
`SessionToolCall.ToolUseID` to make external side effects idempotent.

## Errors and retry safety

Use `errors.As` with `*mango.APIError` to access `StatusCode`, `Type`, and
`RequestID`. Avoid logging full error bodies, which may contain application data.
Generated one-shot methods do not automatically retry writes after an ambiguous
failure. `SessionToolRunner` is the narrow exception: it checks durable result
history before retrying its own transient result submission.

- [Runnable multi-language quickstart](../getting-started.md)
- [Complete SDK README and examples](https://github.com/yanpgwang/mango/tree/main/sdk/go)
