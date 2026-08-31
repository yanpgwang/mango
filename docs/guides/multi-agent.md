---
title: Run a multi-agent Session
slug: /guides/multi-agent
---

# Run a multi-agent Session

This guide creates two worker Agents, places them in a coordinator roster, and
observes the persistent child Threads that the coordinator starts while
handling a Session.

Multi-agent execution uses the ordinary Session and Event APIs. Delegation is
not a client-side endpoint: a coordinator receives the private `list_agents`
and `send_to_agent` model tools and decides when to call them.

For a runnable public-HTTP scenario with a real coordinator, two specialists,
an Advisor, and persistent follow-up, see
[Coordinate a specialist team](../examples/multi-agent-team.md).

## Prerequisites

- Complete [Getting started](../getting-started.md).
- Use the current Messages-shaped model adapter with a model that can call
  tools. The deterministic local
  model is useful for platform smoke tests but does not make open-ended
  delegation decisions.

The examples use the local API at `http://localhost:8080`.
Export `MANGO_API_KEY` with the same Workspace key used in Getting started.
The HTTP snippets below show the wire requests; the first-party
[SDKs](../sdk.md) handle authentication and these resource operations as well.

## Create the worker Agents

```bash
RESEARCHER_ID=$(
  curl -sS http://localhost:8080/v1/agents \
    -H "Authorization: Bearer $MANGO_API_KEY" \
    -H 'content-type: application/json' \
    -d '{
      "name": "researcher",
      "model": "claude-model-id",
      "system": "Investigate the question and return a concise evidence-backed report."
    }' | jq -r .id
)

REVIEWER_ID=$(
  curl -sS http://localhost:8080/v1/agents \
    -H "Authorization: Bearer $MANGO_API_KEY" \
    -H 'content-type: application/json' \
    -d '{
      "name": "reviewer",
      "model": "claude-model-id",
      "system": "Review the proposed answer, identify errors, and return corrections."
    }' | jq -r .id
)
```

Agent names identify callable roster members inside one Session and therefore
must be distinct.

## Create the coordinator

```bash
COORDINATOR_ID=$(
  curl -sS http://localhost:8080/v1/agents \
    -H "Authorization: Bearer $MANGO_API_KEY" \
    -H 'content-type: application/json' \
    -d "{
      \"name\": \"coordinator\",
      \"model\": \"claude-model-id\",
      \"system\": \"Delegate independent work in parallel. When review depends on a research result, send the completed report to the reviewer as a follow-up task, then synthesize the final answer.\",
      \"multiagent\": {
        \"type\": \"coordinator\",
        \"agents\": [\"$RESEARCHER_ID\", \"$REVIEWER_ID\"]
      }
    }" | jq -r .id
)
```

Mango resolves every roster entry to an immutable Agent Version when the
coordinator Version is written. Session creation then expands those pins into
complete Session-owned Agent snapshots, so later Agent updates cannot drift a
running Session.

## Add an optional Advisor

A coordinator may contain one Mango-managed Advisor entry in addition to
ordinary Agents:

```json
{
  "multiagent": {
    "type": "coordinator",
    "agents": [
      {"type":"agent","id":"agent_...","version":1},
      {"type":"advisor","model":"claude-opus-5"}
    ]
  }
}
```

The Advisor is primary-only and invisible to `list_agents` and `send_to_agent`;
the executor decides when to call the ordinary private `advisor({})` tool.
Mango quotes the executor's current system prompt, tool definitions, transcript,
and partial response into a separate tool-free request to the configured model.
Every consultation appears afterward as an automatically terminating
`anthropic.advisor` Thread with its own events and usage. That is the current
reserved name, not a compatibility commitment; the inference itself is
provider-neutral.

## Start and prompt the Session

Create a cloud Environment and Session as in the quick start, using
`COORDINATOR_ID` as the Session Agent. Then send a normal `user.message`:

```bash
curl -sS "http://localhost:8080/v1/sessions/$SESSION_ID/events" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  -H 'content-type: application/json' \
  -d '{
    "events": [{
      "type": "user.message",
      "content": [{"type":"text","text":"Compare two approaches, have the reviewer challenge the result, and give me one recommendation."}]
    }]
  }' | jq
```

The coordinator may create child Threads asynchronously. Each child owns its
Agent snapshot, event ledger, provider transcript, usage, retry state, live
preview stream, and Temporal Workflow while sharing the Session sandbox and
attached resources.

`send_to_agent` does not block the coordinator turn. Independent work can run
in parallel, but dependencies remain the coordinator's responsibility: one
child does not receive another child's future report automatically. Wait for
the prerequisite report to arrive on the primary Thread, then send the
dependent child a self-contained task. Follow-ups can name the existing child
Thread so it continues with its prior context.

## Observe the Threads

List the primary and child Threads:

```bash
curl -sS "http://localhost:8080/v1/sessions/$SESSION_ID/threads" \
  -H "Authorization: Bearer $MANGO_API_KEY" | jq
```

Read one child ledger independently:

```bash
curl -sS \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  "http://localhost:8080/v1/sessions/$SESSION_ID/threads/$THREAD_ID/events" |
  jq
```

The primary Session stream contains condensed lifecycle and directional
message projections. A completed child answer arrives as a report and wakes a
later coordinator synthesis turn; it is not duplicated as a primary
`agent.message`. The runtime preserves `from_agent_name` and
`from_session_thread_id` when it presents that report to the coordinator model,
so the internal Agent message cannot be mistaken for a new user message.

## Answer a child action

If a child needs confirmation or a custom/self-hosted tool result, Mango
projects an answerable event onto the primary Session stream. Reply to that
event ID through the ordinary Session Events endpoint. An optional
`session_thread_id` may be echoed as a routing check; a conflicting value is
rejected.

An interrupt without `session_thread_id` targets the primary and every active
child. Include a child ID to interrupt only that Thread.

## Current limits

- Advisor consultations are recorded after the independent Advisor request returns, so a
  live Advisor Thread and targeted Advisor-only interrupt are not available
  during the quiet inference window.
- Child transcripts are durable and independently compacted. Compacted message
  projections are stored as immutable internal snapshots, and compaction is
  observable through `agent.thread_context_compacted` on the owning Thread.
- Provider usage is recorded per model request on the owning Thread and charged
  atomically to the shared Session list-cost budget. Concurrent requests may
  overshoot because each is admitted before it starts.
- Report-only coordinator turns do not yet have a dedicated preview policy.

See [Session Threads](../api/session-threads.md) for the HTTP contract and
[capabilities and limits](../capabilities.md) for the current support boundary.
