---
title: Gate agent actions on application or human input
slug: /guides/hitl-gate
---

# Gate agent actions on application or human input

Custom tools let an Agent ask your application to perform work that should not
run inside its sandbox. The application can call an internal service, record an
audited decision, or wait for a human reviewer, then return the result to the
same durable Session.

This guide describes the lifecycle contract. Complete
[Getting started](../getting-started.md) first and use the exact request shapes
from the [Agents](../api/agents.md) and [Events](../api/events.md) references.

## Define the gate

A custom tool has a name, description, and JSON Schema. It does not need a
sandbox implementation:

```json
{
  "name": "expense-gate",
  "model": "your-model-id",
  "system": "Use decide for clear expenses and escalate when a human judgment is required.",
  "tools": [
    {
      "type": "custom",
      "name": "decide",
      "description": "Record a final approve or reject decision.",
      "input_schema": {
        "type": "object",
        "properties": {
          "receipt_id": {"type": "string"},
          "action": {"type": "string", "enum": ["approve", "reject"]},
          "reason": {"type": "string"}
        },
        "required": ["receipt_id", "action", "reason"]
      }
    },
    {
      "type": "custom",
      "name": "escalate",
      "description": "Request a human decision for an ambiguous expense.",
      "input_schema": {
        "type": "object",
        "properties": {
          "receipt_id": {"type": "string"},
          "question": {"type": "string"}
        },
        "required": ["receipt_id", "question"]
      }
    }
  ]
}
```

When the model calls either tool, Mango persists an
`agent.custom_tool_use`. It does not execute application or human work on the
model's behalf.

## Observe the barrier

After committing all custom calls from that model round, Mango emits
`session.status_idle` with `stop_reason.type=requires_action`. Its `event_ids`
contains every action that must be answered before the model can continue.

Store your business result idempotently before sending the corresponding
`user.custom_tool_result`. Use the `agent.custom_tool_use` event ID as the
`custom_tool_use_id`; do not invent a separate correlation ID.

Multiple results can be sent in one Events request:

```json
{
  "events": [
    {
      "type": "user.custom_tool_result",
      "custom_tool_use_id": "sevt_decide",
      "content": [{"type": "text", "text": "{\"recorded\":true,\"receipt_id\":\"r01\",\"decision\":\"approve\"}"}]
    },
    {
      "type": "user.custom_tool_result",
      "custom_tool_use_id": "sevt_escalate",
      "content": [{"type": "text", "text": "{\"recorded\":true,\"receipt_id\":\"r02\",\"human_decision\":\"reject\"}"}]
    }
  ]
}
```

Replace `sevt_decide` and `sevt_escalate` with the actual
`agent.custom_tool_use` event IDs returned by the Session.

Partial results remain durable without reopening the Session. The final result
claims the complete barrier, changes the Session to running, and wakes one
resume turn containing every paired tool result. A duplicate or mismatched
result is rejected. An ordinary user message may be accepted during the wait,
but cannot run before the result round completes.

## Recover safely

PostgreSQL, not the API connection or worker memory, owns the barrier. If the
client, API, or execution worker restarts:

1. open the Session event stream;
2. list persisted history while the stream is open;
3. find the latest `requires_action` boundary;
4. correlate its action IDs with already persisted `user.custom_tool_result`
   events;
5. submit only missing results.

Mango does not currently deliver outbound webhooks. Production consumers must
use the documented stream-plus-history recovery pattern or bounded polling.
Webhook delivery remains a separate product feature because it needs durable
retry, signing, idempotency, and delivery observability.

## Executable verification

The documented expense workflow is exercised with a configured real model,
real PostgreSQL, real Temporal, and Mango's durable Session runtime. The model
must emit one `decide` and one `escalate` call, the test client returns the
application and human decisions in two admissions, and the same model must
finish after receiving both correlated results:

```bash
scripts/with-dev-env make test-hitl-gate-live
```

The test client deterministically stands in for the expense system and human
reviewer; no test can automate a literal person. The model interaction and
Mango lifecycle are real, credentialed, and potentially billable.

A separate offline test creates seven parallel actions, answers only three,
rejects a duplicate without a partial commit, replaces the execution worker,
and then resumes the complete barrier exactly once:

```bash
make test-hitl-gate
```

That deterministic test proves failure and concurrency invariants; it is not
presented as the Cookbook user example.

## Design boundary

The expense-approval user problem and custom-tool round trip are informed by
Anthropic's public CMA gate cookbook. Mango uses its own complete-barrier,
PostgreSQL admission, Temporal recovery, and event semantics. See
[Design provenance](../provenance.md) for the adopted and changed parts.
