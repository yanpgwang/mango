---
title: Session Threads
description: Inspect child conversations and manage delegated work.
slug: /api/session-threads
---

# Session Threads

Every Session has a durable primary Thread. A coordinator can start persistent
child Threads through its private model tools. The Session remains the
aggregate resource while every Thread owns its execution and conversation.

```text
GET  /v1/sessions/{session_id}/threads
GET  /v1/sessions/{session_id}/threads/{thread_id}
POST /v1/sessions/{session_id}/threads/{thread_id}/archive
GET  /v1/sessions/{session_id}/threads/{thread_id}/events
GET  /v1/sessions/{session_id}/threads/{thread_id}/stream
```

The primary identity and its initial execution projection are inserted in the
same PostgreSQL transaction as the Session, its immutable Skill and Vault pins,
initial events, and orchestration outbox. Existing databases receive
deterministic primary identities and backfilled projections during migration.
Deleting a Session cascades to its Threads.

## Projection model

Each Thread owns an independent PostgreSQL projection of its immutable Agent
snapshot, status, cumulative usage, and timing. The primary Thread has a null
`parent_thread_id`. Its Agent omits `multiagent`; the resolved coordinator
roster remains on `Session.agent` as part of the current Mango response shape.

The runtime updates an owning Thread before recomputing the Session aggregate in
the same PostgreSQL transaction. The Session remains running while any Thread
is running; usage is the sum of independently accumulated Thread usage.
Session-only title, metadata, and resource changes do not mutate Thread state.

Thread Event list and stream select the chosen Thread from the Session-wide
ordered event ledger. Pagination uses forward-only opaque cursors bound to both
Session and Thread IDs. Streaming supports the same opt-in `event_deltas[]`
values as the Session stream, but child previews use Thread-scoped NATS subjects
and never leak into the primary stream.

Archiving the primary Thread uses the Session's idle-only archive fence. A
running Thread returns `409`; after archive the Thread reports `terminated` and
its duration is frozen.

Archiving a child uses its own idle/rescheduling lifecycle fence and never
archives the aggregate Session. PostgreSQL atomically freezes the child
projection, closes its unresolved client-action barrier, flushes queued input,
emits `session.thread_status_terminated` on the child and primary ledgers, and
upgrades the child orchestration outbox to a termination intent. That intent
dominates a stale wake for the same Thread, and the relay retries the idempotent
Temporal termination until it succeeds. Session deletion installs the same
intent for every child before stopping the primary Workflow and releasing the
sandbox.

## Coordinator execution

A coordinator receives `list_agents` and `send_to_agent` as private model tools;
they do not emit generic `agent.tool_use`/`agent.tool_result` events. A new send
atomically captures the resolved roster Agent, creates the child projection,
appends directional message and lifecycle events, and writes the child outbox.
The relay starts a stable per-Thread Temporal Workflow. Follow-up sends reuse
that Thread and its provider-native transcript. A completed child answer is
reported asynchronously to the primary Thread, which is then scheduled for a
separate synthesis turn. The model-facing projection wraps the report with its
`from_agent_name` and `from_session_thread_id`; providers still receive a legal
user-role input, while the coordinator can distinguish internal Agent traffic
from user-authored messages and address follow-ups to the existing Thread.

## Client-action routing

When a child requires a tool confirmation, custom-tool result, or self-hosted
tool result, its canonical action and `session.thread_status_idle` boundary are
written to the child ledger. The server cross-posts a client-visible copy to
the primary stream with `session_thread_id`, and the aggregate
`session.status_idle` references those visible event IDs.

Clients answer the cross-posted event ID using the ordinary
`user.tool_confirmation`, `user.custom_tool_result`, or `user.tool_result`
shape. They may echo `session_thread_id` as a redundant routing hint. The
server resolves the event ID to its owning Thread, rejects a conflicting hint,
persists the result in the child ledger, and wakes only that child after every
action in the barrier has a result. A companion `system.message` follows the
same route. A waiting child does not block work on the primary or a sibling,
and it emits no terminal report until its barrier is resolved.

Interrupts use the ordinary Session Events endpoint. Omitting
`session_thread_id` durably fans the control event out to the primary and every
active child ledger; providing it targets and wakes only the named Thread.
Every Thread independently applies the same PostgreSQL finish-vs-interrupt
ordering fence, active-Activity cancellation, and idle no-op behavior.

## Advisor consultations

When the primary model invokes Mango's private Advisor client tool, Mango runs
the configured model as an independent, tool-free inference and records
each consultation as an automatically terminating child Thread named
`anthropic.advisor`. The Thread contains the configured
`{"type":"advisor","model":"..."}` identity, has the primary Thread as its
parent, and is visible through the same Thread list, get, and event endpoints.
It is not a persistent callable Agent, does not appear in `list_agents`, cannot
receive `send_to_agent`, and is excluded from the concurrent persistent-Thread
limit.

Successful consultations project created, running, message, idle, and
terminated events onto the Advisor and primary ledgers. A failed consultation
still records its lifecycle, returns an error tool result to the executor, and
does not invent a delivered message. Advisor token usage and list cost belong
to the Advisor Thread and are also added atomically to the shared Session usage
and budget. The Advisor projection and private tool result commit in one
transaction, so an Activity retry cannot repeat a billed inference whose result
was already recorded.

The runtime currently commits the consultation lifecycle after the Advisor
request returns. It therefore does not expose a live Advisor Thread during the
quiet inference window or support a targeted in-flight Advisor-only interrupt.
A global interrupt still cancels the active tool Activity.

Compacted child context projections are durably checkpointed, and
`agent.thread_context_compacted` is emitted on the owning Thread ledger. The
remaining multi-agent boundary includes exact hosted preview suppression for
report-only turns and the live Advisor timing/interrupt boundary above. Token
and provider-native Web tool usage is owned by each independent Thread and also
posted atomically to the shared Session list-cost budget.

Start with the [multi-agent guide](../guides/multi-agent.md).
