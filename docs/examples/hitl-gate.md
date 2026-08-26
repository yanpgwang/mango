---
title: Gate agent actions on application or human input
slug: /examples/hitl-gate
---

# Gate agent actions on application or human input

Custom tools let an Agent ask your application to perform work that should not
run inside its sandbox. The application can call an internal service, record an
audited decision, or wait for a human reviewer, then return the result to the
same durable Session.

This example includes a runnable program over Mango's public HTTP API. Mango does
not yet publish an SDK, so the example deliberately uses Go's standard
`net/http` client and exposes the same requests an SDK would eventually wrap.
It never calls the model provider directly: the configured Mango worker owns
that credential and model request. Complete [Getting started](../getting-started.md)
first and use the exact request shapes from the [Agents](../api/agents.md) and
[Events](../api/events.md) references.

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

An application may subscribe to `session.status_idled` through
[Webhooks](../api/webhooks.md) to start this recovery promptly. The notification
is deliberately thin, at least once, and not an infinite log: production
consumers must still use the documented stream-plus-history recovery pattern or
bounded reconciliation after receiving it.

## Executable verification

Start the local stack with the real model values in
`~/.config/mango/dev.env`, then run the interactive example:

```bash
make local-up
make local-health
scripts/with-dev-env make demo-hitl-gate
```

The program creates an Environment, Agent, and Session through public HTTP,
sends two receipts, and waits for the complete `requires_action` barrier. The
real model must produce `decide` for the clear receipt and `escalate` for the
ambiguous one. The application records `decide` automatically; for `escalate`,
the terminal displays the model's question and waits for you to type
`approve` or `reject`. Results are submitted separately, and the program waits
for the real model's final response after the complete barrier.

The runnable source is
[`examples/hitl-gate`](https://github.com/yanpgwang/mango/tree/main/examples/hitl-gate).
It defaults to `http://localhost:8080` and the documented local Mango API key.
Set `MANGO_EXAMPLE_BASE_URL`, `MANGO_API_KEY`, or
`MANGO_EXAMPLE_MODEL_ID` to override them. Set
`MANGO_EXAMPLE_KEEP_RESOURCES=1` to retain the created resources for history
inspection; otherwise the program deletes the Session and Environment and
archives the Agent after success. The Make target removes model-provider
credentials from the example process environment before starting it.

A successful run resembles the following. Model wording can vary, but the
program requires the two typed actions and the final completed turn:

```text
The real model requested these application-owned actions:
  decide {"action":"approve",...,"receipt_id":"r01"}
  escalate {"question":"This USD 900 expense ... can an itemized receipt be provided?","receipt_id":"r02"}

Application records approve for r01.
First result persisted; the Session remains at the incomplete barrier.

Human review required for r02: This USD 900 expense ... can an itemized receipt be provided?
Decision [approve/reject]: reject

Agent final response:
Summary of recorded outcomes:
Receipt r01: Approved ...
Receipt r02: Rejected (after human review) ...
```

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
