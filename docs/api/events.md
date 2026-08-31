---
title: Events and streaming
---

# Events and streaming

Events are flat tagged-union objects. The `type` field selects the remaining
shape; persisted events receive an `id` and `processed_at`.

## Send events

`POST /v1/sessions/{id}/events`

```json
{
  "events": [{
    "type": "user.message",
    "content": [{"type": "text", "text": "Inspect the failure"}]
  }]
}
```

The list must be non-empty. Clients cannot provide `id` or `processed_at`, and
server-emitted event types are rejected.

The PostgreSQL/Temporal control plane currently accepts:

| Event | Current behavior |
| --- | --- |
| `user.message` | Starts a model turn |
| `user.interrupt` | Cancels active work, or is acknowledged as an idle no-op; omit `session_thread_id` to interrupt every non-archived Thread, or provide it to target one Thread |
| `user.custom_tool_result` | Supplies a result for a pending custom tool call |
| `user.tool_result` | Supplies a client-executed built-in result for a `self_hosted` environment |
| `user.tool_confirmation` | Allows or denies a pending `always_ask` built-in |
| `user.define_outcome` | Starts outcome work and independent evaluation/revision cycles |
| `system.message` | Text-only companion context; must be the final event immediately after a message or tool result |

`user.tool_result` is rejected unless it resolves a pending self-hosted
`agent.tool_use`. A tool confirmation or result may include
`session_thread_id` when answering a child action cross-posted onto the primary
stream. The event reference is authoritative for routing; the hint is optional
and a conflicting value is rejected. The persisted response and any companion
`system.message` belong to the child Thread.

### Handle required client actions

A model response may emit multiple custom tools or confirmation-gated tools in
one round. Mango commits every action and one `session.status_idle` whose
`stop_reason` contains the complete barrier:

```json
{
  "type": "session.status_idle",
  "stop_reason": {
    "type": "requires_action",
    "event_ids": ["sevt_action_1", "sevt_action_2"]
  }
}
```

Answer an `agent.custom_tool_use` by copying its event ID into
`custom_tool_use_id`:

```json
{
  "events": [{
    "type": "user.custom_tool_result",
    "custom_tool_use_id": "sevt_action_1",
    "content": [{"type": "text", "text": "{\"recorded\":true}"}]
  }]
}
```

Results may be submitted separately or in one batch. A partial submission is
durably claimed but leaves the Session idle; Mango resumes the model exactly
once only after every ID in that barrier has a matching result. A duplicate
result returns `409` and commits none of the failing request. If a client loses
the HTTP response, it should list persisted events and correlate the action ID
before retrying rather than assuming the result was rejected.

### Approve externally executed tools

Execution location does not change permission policy. A self-hosted built-in
with `always_ask` emits `agent.tool_use` with `evaluated_permission: "ask"` and
waits for `user.tool_confirmation` referencing that original event ID.

- `allow` authorizes the external worker to execute the original call. Mango
  persists the approval and stamps its `processed_at` on admission, but keeps
  the action unresolved; the waiting Thread does not resume until its complete
  result barrier is ready, including this call's `user.tool_result`.
  The result uses the same `tool_use_id`; no replacement tool-use event is emitted.
- `deny` resolves the call without external execution or a client tool result.
  When the complete barrier is ready, Mango gives the model a correlated error
  result, including any `deny_message`. It never executes the denied tool.
- A result before approval, or after denial, is rejected with `400`. Repeated
  approvals and duplicate results return `409`. A failed batch commits nothing;
  approval/result validation follows the submitted event order.

Workers must wait for a committed allow before executing, and must never
interpret approval as completion. After reconnecting, reconcile the original
tool use, confirmation, and result from history. For a cross-posted child call,
the confirmation and result live in the owning Thread's history, both routed
using the original client-visible ID. Unapproved calls remain blocked, and an
existing result must not be blindly executed again. Workspace keys authorize
trusted workers; Mango does not sandbox application-side execution or guarantee
exactly-once external side effects.

The barrier lives in PostgreSQL and is selected before ordinary queued input.
Worker or API replacement does not discard it, and a message accepted during
the wait cannot overtake the complete result round. See the
[human-in-the-loop gate example](../examples/hitl-gate.md) for the full workflow.

An interrupt without `session_thread_id` is admitted to the primary and every
active, non-archived child Thread. Supplying `session_thread_id` admits it only
to that Thread and wakes only its Workflow. An unknown or cross-Session Thread
ID is rejected, as is a direct target that can no longer execute. The send
response still contains exactly one event for each caller-submitted input;
server-created fan-out receipts remain internal to their owning Thread ledgers.

Content blocks are validated as closed tagged unions. Images accept `base64`
and `url` sources. Documents accept `base64`, `text`, `url`, and `file`
sources, with `text` sources requiring `media_type: text/plain`. A `file`
source is supported only on `user.message` documents and must reference a
ready, top-level File in the same Workspace whose declared media type and bytes
are eligible for UTF-8 text projection. Mango checks the stored size and
SHA-256, rejects empty, NUL-containing, non-UTF-8, scoped, missing, corrupt,
and non-text Files, and limits resolved File content to 262,144 characters per
admission. The private text snapshot is committed with the event and projected
to the model as an ordinary text block; the public event continues to expose
only the documented `file_id`. File-sourced images and tool-result documents
return `422 unsupported_error`.
Tool-result search blocks require `source`, `title`, `citations.enabled`, and
an array of text blocks. Unknown fields are rejected at every nested level.
Outcome rubrics accept inline `{type: "text", content: "..."}` or reusable
`{type: "file", file_id: "file_..."}` inputs. Both are limited to 262,144
characters. A File rubric must be a ready, top-level File in the same Workspace;
Mango validates and snapshots its UTF-8 text before admitting the event. The
public event keeps the File reference and never exposes the private snapshot.

The current Outcome grader makes an isolated, tool-free model request using
the rubric and candidate conversation evidence. It cannot independently open
an output File, fetch a URL, or run a test. A passing evaluation is therefore
a judgment of the supplied evidence, not independent artifact verification.
Applications requiring artifact acceptance should download and check the
outputs separately.

An interrupt is first committed to PostgreSQL and then delivered to each
affected Workflow as a metadata-only wakeup. An interrupt that commits before
turn completion wins that ordering point: the owning execution ends with
exactly one idle boundary whose stop reason is `end_turn`. If completion
commits first, a later interrupt is an idle control event. A batch may place a
new `user.message` after `user.interrupt` to redirect the primary Session into
another turn.

The response echoes only the submitted events:

```json
{"data": []}
```

Status and agent output are asynchronous and appear in list/stream results.

## List events

`GET /v1/sessions/{id}/events`

Supported query parameters:

| Parameter | Meaning |
| --- | --- |
| `limit` | Page size, `1`–`1000`; default `100`. Values above `1000` return a validation error. |
| `order` | `asc` or `desc`; default `asc` |
| `page` | Opaque forward cursor |
| `types[]` | Repeatable event type filter |
| `created_at[gt\|gte\|lt\|lte]` | RFC 3339 bounds applied to `processed_at` |

```json
{
  "data": [{
    "id": "sevt_...",
    "type": "agent.message",
    "content": [{"type": "text", "text": "Done"}],
    "processed_at": "2026-07-27T00:00:01Z"
  }],
  "next_page": null
}
```

Ordering, timestamp bounds, and cursors use `processed_at`, matching the public
contract despite the current query name `created_at`. Ascending order
places processed events first and unprocessed (`null`) events last; descending
order reverses that placement. The internal receipt sequence is used only as a
stable tie-breaker for equal or null timestamps and is never exposed.

## Stream events

`GET /v1/sessions/{id}/events/stream`

The endpoint returns `text/event-stream`. Persisted frames use their event type
as the SSE discriminator:

```text
event: agent.message
data: {"id":"sevt_...","type":"agent.message","content":[...],"processed_at":"..."}
```

The stream starts after the latest committed event at subscription time. It does
not replay earlier history and does not implement `Last-Event-ID`.

For reconnect without gaps:

1. open a new stream;
2. list persisted history while the stream is open;
3. merge both sources and deduplicate by event `id`.

An active stream receives `session.deleted` and then EOF when its session is
deleted.

### SDK streaming example

The examples use a newly created Session and the configured client from
[Getting started](../getting-started.md). SDKs open a live subscription before
sending; HTTP polls this new Session's first turn. They check the stop reason
and close the subscription. Do not reuse this first-turn polling shortcut to
identify completion of a later turn in existing history.

::include[../../sdk/typescript/examples/quickstart.ts#stream]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../../sdk/python/examples/quickstart.py#stream]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../../sdk/go/examples/quickstart/main.go#stream]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../../examples/sdk-quickstart.sh#stream]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

### Read history in your language

::include[../../sdk/typescript/examples/quickstart.ts#history]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../../sdk/python/examples/quickstart.py#history]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../../sdk/go/examples/quickstart/main.go#history]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../../examples/sdk-quickstart.sh#history]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Live event previews

Opt in to ephemeral assistant text and privacy-safe thinking starts:

```http
GET /v1/sessions/{id}/events/stream?event_deltas[]=agent.message
```

Repeat `event_deltas[]` with `agent.thinking` to receive thinking starts too.

The stream may first emit:

```text
event: event_start
data: {"type":"event_start","event":{"type":"agent.message","id":"sevt_..."}}

event: event_delta
data: {"type":"event_delta","event_id":"sevt_...","delta":{"type":"content_delta","index":0,"content":{"type":"text","text":"partial"}}}
```

The preview and eventual persisted `agent.message` share the same event ID.
An `agent.thinking` preview emits only `event_start`; it never emits reasoning
content or an `event_delta`. Its ID is reused by the privacy-preserving
persisted `agent.thinking` event.
Preview frames:

- are delivered only to opted-in subscribers;
- are never written to the event log;
- never appear in list results;
- may end without an authoritative event if generation or the process fails.

If generation is interrupted after preview delivery, the terminal
`span.model_request_end` closes the preview even when no buffered
`agent.message` is produced.

Model request span IDs are allocated before provider execution. The durable
`span.model_request_start` is appended before its provider call can publish a
preview; the authoritative message and correlated `span.model_request_end`
follow when that model round completes. If a preview arrives before its
persisted-event wakeup, the stream reconciles its PostgreSQL cursor before
forwarding the first preview frame.

An outcome evaluation durably publishes `span.outcome_evaluation_start` before
the grader runs and emits periodic `span.outcome_evaluation_ongoing` events
while it remains active. Its terminal `span.outcome_evaluation_end` references
the start event. This correlation is preserved when an active grader is
interrupted. If interruption happens before any evaluation start can be
published, the documented `outcome_evaluation_start_id` is the empty string.
Completed `needs_revision` evaluation pairs remain in history and the
interrupt end uses the next zero-based iteration.

## Backpressure

NATS Core carries best-effort wakeups and previews across API/worker processes;
PostgreSQL remains authoritative. Each subscriber periodically reconciles its
durable PostgreSQL cursor, so a lost wakeup delays a persisted event but does
not lose it. The output buffer is bounded: a slow subscriber is disconnected
and should reconnect using the open-stream-then-list procedure above. Preview
frames are ephemeral and can be lost. A replacement API process opens a new
subscription after the latest committed event; listing history after that
stream is open fills the process-restart gap without replaying old events on the
stream itself.
