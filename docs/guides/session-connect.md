---
title: Connect to a Session
description: Follow durable history, send messages, and answer approvals from a terminal.
---

# Connect to a Session

Use `mango sessions connect` when you already have a Session and want to follow
it from a terminal. The command talks to your Mango HTTP API through the Mango
Go SDK. It needs a Workspace API key in `MANGO_API_KEY`; it does not need database
access, a model key, or a hosted agent account.

## Start a connection

Build the current checkout with `make build`. Use the Workspace key and Session
ID from the [Quickstart](../getting-started.md), then run:

```bash
export MANGO_BASE_URL=http://localhost:8080
./bin/mango sessions connect sesn_YOUR_SESSION_ID
```

Keep `MANGO_API_KEY` in your configured environment or secret runner. The command
does not accept a key flag. Use HTTPS when connecting across a network.
`MANGO_BASE_URL` defaults to `http://localhost:8080`; `-base-url URL` overrides it.
Flags follow the Session ID:

```bash
./bin/mango sessions connect sesn_YOUR_SESSION_ID -read-only
./bin/mango sessions connect sesn_YOUR_SESSION_ID -verbose
./bin/mango sessions connect sesn_YOUR_SESSION_ID -h
```

The command prints all pages of saved primary Session history, then follows
live durable events. Messages and text tool results are printed as text. Tool
calls include their arguments and MCP server name; status events include stop
reasons and errors. `-verbose` prints full event JSON, including content that
may be sensitive. Terminal control characters are removed from displayed text.
Ephemeral preview deltas are not requested.

## Send input and handle approvals

Type a line and press Enter to send a text message. Input is limited to 1 MiB
per line. Commands are:

| Input | Effect |
| --- | --- |
| `/allow sevt_ACTION_ID` | Allow the pending tool or MCP action. |
| `/deny sevt_ACTION_ID [reason]` | Deny that action, optionally explaining why. |
| `/interrupt` | Explicitly interrupt every non-archived Session Thread. |
| `/message TEXT` | Send text literally, including a message starting with `/`. |
| `/help` | Print the available commands. |
| `/quit` | Detach from the Session. |

An approval prompt looks like:

```text
[agent.tool_use] sevt_ACTION_ID bash
Input: {"command":"ls"}
Approval pending: sevt_ACTION_ID (bash). /allow sevt_ACTION_ID or /deny sevt_ACTION_ID [reason]
```

Use the **action's event ID**, not the ID of a status or confirmation event.
Before showing initial prompts and sending a decision, the command reconciles
saved history. For relayed child actions, it also reads the owning child Thread's
history and preserves its `session_thread_id` in the confirmation. Resolved or
unknown actions are not submitted; the API independently validates admission.
Another operator can resolve an action after this check, in which case the API
can reject the decision.

An approval is permission to proceed, not an execution result. Your self-hosted
worker still owns built-in execution; application-owned custom tools still need
application results. The command does not execute tools or synthesize results.
See [Events and streaming](../api/events.md#handle-required-client-actions).

## Detach and reconnect

Ctrl+C, `/quit`, and input EOF detach without interrupting the Session. Use
`/interrupt` explicitly when you want to cancel work. `-read-only` ignores input
and follows until the Session terminates or you cancel the connection. An
already terminated Session prints history and exits without opening a stream.

After a lost stream or a retryable read failure, the command reconnects and
rechecks every history page. Durable event IDs suppress overlap, including
messages accepted during the disconnect. Mango's SSE endpoint does not replay
history or interpret `Last-Event-ID`; recovery uses the persisted event ledger.
Authentication and other non-retryable read errors end the connection.

**Writes are never retried automatically.** If sending input returns an error or
an invalid receipt, the command exits: the server might already have committed
the event. Reconnect in read-only mode and inspect history before deciding
whether to resend. The command cannot establish receipt of a write after a lost
response.

This is a line-based operator client. It follows the primary stream and handles
child actions relayed there; it is not a viewer of every child conversation or a
shell attached to a sandbox. For targeted Thread controls and worker evidence,
use the [Session troubleshooting guide](session-troubleshooting.md).
