---
title: Troubleshoot a Session
description: Find what a Session is waiting for and decide how to continue from durable history.
---

# Troubleshoot a Session

When an Agent appears stuck, inspect its current Session, persisted events, and
execution lease. These are existing Mango APIs; no model call is needed to read
them. Use an operator's **Workspace key** for this guide. An Environment
supervisor key permits Poll, Ack, and Stats only and cannot read Session history
or list Work.

## Read the current state and complete history

Use a terminal with `curl` and `jq`. Set the URL, Workspace credential, and the
ID returned when your application created the Session:

```sh
export MANGO_BASE_URL=http://localhost:8080
export MANGO_API_KEY=your-workspace-key
export MANGO_SESSION_ID=sesn_...

curl -fsS "$MANGO_BASE_URL/v1/sessions/$MANGO_SESSION_ID" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  | jq '{id, status, environment_id, usage}'
```

Session status narrows the question:

| Status | What it tells you | Next check |
| --- | --- | --- |
| `running` | Work is accepted; it may still be queued or waiting for execution. | Recent events, child Threads, then Environment Work. |
| `rescheduling` | A model request is waiting for an automatic retry. | `session.error.error.retry_status` and later lifecycle events. |
| `idle` | No turn is currently running. This can be an input wait or an action/budget pause. | The latest applicable `session.status_idle.stop_reason`. A new Session may have no turn history yet. |
| `terminated` | The Session cannot accept further work. | Terminal errors or an explicit archive; inspect retained history before deletion. |

Read **every page**. An early error or idle event is not the current state when
later events show recovery. This helper follows each response's opaque
`next_page` without changing filters:

```sh
mango_list_all() (
  mango_path=$1
  shift
  mango_page=""
  while :; do
    mango_response=$(curl -fsS -G "$MANGO_BASE_URL$mango_path" \
      -H "Authorization: Bearer $MANGO_API_KEY" \
      --data-urlencode 'limit=100' --data-urlencode "page=$mango_page" "$@") || exit 1
    printf '%s\n' "$mango_response" | jq -c '.data[]' || exit 1
    mango_page=$(printf '%s\n' "$mango_response" | jq -r '.next_page // empty') || exit 1
    [ -n "$mango_page" ] || break
  done
)

mango_list_all "/v1/sessions/$MANGO_SESSION_ID/events" --data-urlencode 'order=asc' \
  | jq '{id, type, processed_at, stop_reason, error}'
```

Events are ordered by `processed_at`; queued inputs with a null timestamp come
last in ascending order. A queued `user.message` is not proof that its turn has
started. Re-read the Session and history as work progresses: separate GETs are
not one atomic snapshot. See [Events and streaming](../api/events.md) for
filtering and for reconciling a live stream with persisted history.

## Interpret the pause or error

For an idle Session, use the most recent idle boundary that has not been
superseded by running, rescheduled, or terminated events:

| `stop_reason.type` | Meaning and next step |
| --- | --- |
| `requires_action` | Read each event in `stop_reason.event_ids`. Answer custom calls with `user.custom_tool_result`; built-in permission asks need `user.tool_confirmation`, and an allowed self-hosted call still needs `user.tool_result`. Correlate approval and completion separately by the original action ID; all blocking actions must be resolved. |
| `end_turn` | The turn finished or was interrupted. Inspect the final Agent message and your application's success criteria; this is not a guarantee that the overall task succeeded. Send a new message only when you intend a new turn. |
| `retries_exhausted` | Automatic model retries have stopped and later queued messages were flushed. Inspect the error and fix the provider/configuration issue before deliberately submitting a new prompt. |
| `budget_reached` | The shared budget paused further model calls. Review usage and the [Session budget controls](../api/sessions.md). |

For a self-hosted built-in, a persisted `allow` only authorizes execution; it
does **not** complete the action. If approval exists without a matching
`user.tool_result`, inspect the worker and wait for that result instead of
approving or executing again. A `deny` resolves the call without execution or a
tool result. See [Approve externally executed tools](../api/events.md#approve-externally-executed-tools).

Copy a blocking ID from `stop_reason.event_ids` to inspect its call, approval,
and result together:

```sh
export MANGO_ACTION_ID=sevt_...
mango_list_all "/v1/sessions/$MANGO_SESSION_ID/events" --data-urlencode 'order=asc' \
  | jq --arg action "$MANGO_ACTION_ID" \
    'select(.id == $action or .custom_tool_use_id == $action or .tool_use_id == $action)'
```

For a child action, apply this selection to the owning Thread's event list
described below; its approval and result live there.

A `session.error` describes a failure at a particular point in history. Its
`error.retry_status.type` determines the immediate response:

- `retrying`: Mango retries automatically. Wait for later events; do not submit
  the original prompt or tool result again. The error stays in history after a
  successful retry.
- `exhausted`: that turn has stopped retrying. Look for the idle boundary and
  handle the failure in your application.
- `terminal`: inspect the message and the terminated boundary. A permanent
  self-hosted input failure uses `session_input_failed_error`.
- Missing or unrecognized status: inspect the message and current lifecycle;
  do not assume an automatic retry.

Tool failures have their own result events. Inspect the matching
`agent.tool_result` or `agent.mcp_tool_result` and the subsequent Agent response;
a failed tool call does not by itself terminate a Session. When a tool-result
POST has an ambiguous response, reconcile persisted history by action ID before
retrying. The [approval example](../examples/hitl-gate.md) demonstrates retaining
local decisions across that recovery.

For multi-agent work, list the Threads and read the affected child's own ledger:

```sh
mango_list_all "/v1/sessions/$MANGO_SESSION_ID/threads" \
  | jq '{id, parent_thread_id, status, usage}'
export MANGO_THREAD_ID=sthr_...
mango_list_all "/v1/sessions/$MANGO_SESSION_ID/threads/$MANGO_THREAD_ID/events" \
  --data-urlencode 'order=asc' | jq '{id, type, processed_at, stop_reason, error}'
```

The primary stream includes child lifecycle and reports, but each child owns
its detailed conversation and tool history. See [Session Threads](../api/session-threads.md).

## Check self-hosted execution

Copy `environment_id` from the Session response. Work history can contain
multiple activations for the same Session, so correlate `data.id` and inspect
their timestamps:

```sh
export MANGO_ENVIRONMENT_ID=env_...
mango_list_all "/v1/environments/$MANGO_ENVIRONMENT_ID/work" \
  | jq --arg session "$MANGO_SESSION_ID" \
    'select(.data.type == "session" and .data.id == $session)
     | {id, state, created_at, acknowledged_at, started_at,
        latest_heartbeat_at, stop_requested_at, stopped_at}'
```

| Work state | Next check |
| --- | --- |
| `queued` | Verify that a supervisor is polling this Environment with a valid Environment key and can reach the API. A tentative claim can still be queued until Ack. |
| `starting` | Check supervisor/worker logs for sandbox startup and Session input preparation; Ack alone does not prove a sandbox started. |
| `active` | Check whether `latest_heartbeat_at` advances, then correlate tool events and worker logs. A heartbeat proves lease renewal, not model/tool progress. |
| `stopping` / `stopped` | Inspect the stop timestamps and Session history. An old stopped activation does not describe a newer queued one. |

Lease loss is recovered by expiry and reclaim, which rotates the per-Work
credential. Work's public timestamps do not expose the full current lease
deadline; a single old heartbeat cannot tell you whether an owner was fenced.
Compare successive reads with worker logs. Avoid manually calling Poll while
inspecting: it claims Work. Follow the [worker guide](self-hosted-worker.md) and
[Work protocol](../api/environment-work.md) for recovery and bounded
healthchecks. API [health probes](../deployment.md#health-probes) establish API
readiness separately from worker execution.
