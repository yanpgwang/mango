---
title: Environment Work
slug: /api/environment-work
---

# Environment Work

Environment Work is Mango's activation and lease protocol for `self_hosted`
Environments. It is not a second Agent runtime. A worker claims a Session,
listens to the existing Session
event stream, executes `agent_toolset_20260401` tools in customer-hosted
infrastructure, and posts results through the existing `user.tool_result`
event.

Mango creates a Work item only when a self-hosted Session has runnable input.
The Work insert, public event admission, Session projection update, and Temporal
outbox wakeup commit in one PostgreSQL transaction. Further runnable input is
coalesced while the Session has a live Work item; input received after Stop
creates a new activation.

## Worker flow

```text
Poll -> Ack -> Heartbeat(NO_HEARTBEAT) -> Heartbeat(previous timestamp) -> Stop
 queued   starting             active                         stopping/stopped
```

- `Poll` tentatively claims the oldest available item. A stale unacknowledged
  claim may be reclaimed. The optional `worker_id` query parameter contributes
  to queue statistics and operational correlation; it is not a credential.
- `Ack` removes the item from the queue and changes it from `queued` to
  `starting`.
- The first heartbeat uses `expected_last_heartbeat=NO_HEARTBEAT`. Every later
  heartbeat echoes the exact timestamp returned by the previous response. A
  mismatch returns `412` so a worker that lost its lease stops executing.
- Graceful Stop changes active Work to `stopping`; the next heartbeat tells the
  worker to cancel. Forced Stop immediately records `stopped`.

The API exposes Get, Update, List, Ack, Heartbeat, Poll, Stats, and Stop beneath:

```text
/v1/environments/{environment_id}/work
```

Stop returns `204 No Content`, and an empty Poll returns an empty JSON object.

## Skills and Session state

The Work and Session event APIs provide the worker protocol; Mango does not
currently ship an Environment worker runner or external Skill activation.
Applications implementing a worker own tool execution, workspace preparation,
and lease-loss cancellation.

Workers must honor `evaluated_permission` independently of execution location.
An `ask` call waits for a persisted allow confirmation; a deny must never run.
After allow, execute once and submit `user.tool_result` for the original call.
An approval alone does not clear the Session's pending-action barrier. On
reconnect, read confirmations and results as well as tool uses, including the
owning Thread history for child calls. See
[external tool approvals](events.md#approve-externally-executed-tools).

Creating a self-hosted Session with custom Skills returns `422` with
`error.type: "invalid_request_error"` before any Session, Work item, or execution
wakeup is created. Admission checks the effective primary Agent and every
resolved roster member after Session overrides. Clearing the primary Agent's
Skills does not clear Skills on independently referenced roster Agents.

Skills storage and Agent definitions remain available when configured, but
neither object storage nor a Skill-capable cloud adapter enables external
worker Skill execution. A self-hosted Session without Skills can use the Work
protocol. See [Skills](skills.md) for Mango-managed sandbox support.

## Security boundary

Workers send a Workspace API key as a bearer credential for Work, Session,
event, and Skill requests. Mango limits all of those resources to the same
Workspace. It does not issue narrower Environment-worker credentials, so
`worker_id` is not an authorization boundary and Work `secret` remains
`null`. A surrounding control plane should add Environment-specific policy
before exposing this surface to untrusted workers.

See [capabilities and limits](../capabilities.md) for the current support boundary.
