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
or `user.custom_tool_result` event.

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

- `Poll` tentatively claims the oldest available item and returns a fresh Work
  `secret`. It is a URL-safe base64 JSON payload containing the claim's
  `sessions_token`. A stale unacknowledged claim may be reclaimed; every reclaim
  rotates the token. The token becomes usable after Ack; the tentative Poll
  response alone does not authorize Session execution. The optional `worker_id`
  query parameter contributes to queue statistics and operational correlation;
  it is not a credential.
- `Ack` removes the item from the queue and changes it from `queued` to
  `starting`. It uses the polling client's Workspace credential and has no
  request body. Repeating Ack after a successful transition is idempotent, so a
  lost success response can be retried.
- The first heartbeat uses `expected_last_heartbeat=NO_HEARTBEAT`. Every later
  heartbeat echoes the exact timestamp returned by the previous response.
  `desired_ttl_seconds`, when supplied, must be from 1 through 300. Healthy
  workers renew continuously; the five-minute cap bounds stale-owner access.
  Heartbeat and the worker's final Stop authenticate with `sessions_token`. A
  stale timestamp or expired current lease returns `412`; reclaim rotates the
  credential, so an old owner is rejected at authentication.
- Graceful Stop changes active Work to `stopping`; the next heartbeat tells the
  worker to cancel. A stopping token expires no later than its current TTL even
  if no poller performs the eventual state cleanup. Forced Stop immediately
  records `stopped`. A Workspace API key retains operator authority to stop Work
  without possessing its Session credential.

The API exposes Get, Update, List, Ack, Heartbeat, Poll, Stats, and Stop beneath:

```text
/v1/environments/{environment_id}/work
```

Stop returns `204 No Content`, and an empty Poll returns an empty JSON object.
Get, List, metadata Update, and Ack responses redact the Work secret as `null`;
only Poll returns the raw payload. The Go WorkPoller preserves the polled value
in memory when it returns the acknowledged item.

## Skills and Session state

The Work and Session event APIs provide the worker protocol. The Go SDK ships a
provider-neutral `WorkPoller` for poll, Ack, drain, and reclaim, plus a
single-Session `SessionToolRunner` for stream/history recovery, confirmation
gates, local dispatch, and result submission. `EnvironmentWorker` composes both
with conditional heartbeat, lease-loss cancellation, the scoped Work-secret
handoff, and final forced Stop. `Run` owns Poll through Stop in one trusted
process; `HandleItem` runs only an already-acknowledged item and can read its
narrow identity from `MANGO_WORK_ID`, `MANGO_ENVIRONMENT_ID`,
`MANGO_SESSION_ID`, and `MANGO_WORK_SECRET` inside a launcher-created sandbox.

These SDK lifecycle helpers do not choose or create a sandbox and do not
prepare File, Git, Skill, or Memory inputs. Mango's first-party Docker launcher
composes them with container and workspace-volume lifecycle; other launchers
still own that boundary. Mango does not currently ship external Skill
activation. See the staged
[self-hosted worker design](../architecture/self-hosted-workers.md).

## First-party Docker worker

Build and run the preview reference worker from the repository root:

```sh
docker build -f deployments/self-hosted/docker/Dockerfile \
  -t mango-self-hosted-worker:local .

MANGO_API_KEY=replace-with-a-workspace-key \
MANGO_ENVIRONMENT_ID=env_replace_me \
MANGO_BASE_URL=http://localhost:8080 \
MANGO_DOCKER_BASE_URL=http://host.docker.internal:8080 \
go run ./cmd/mango-worker docker
```

The supervisor uses the Workspace key only for Poll and Ack. It creates a
hardened container for each acknowledged Work item and injects only the opaque
Work secret plus resource IDs and the sandbox-visible Mango URL. The item
process performs the first heartbeat before executing tools, continuously
renews the lease, reconciles Session events, posts tool results, and force-Stops
ordinary exits. It runs the Go SDK's six core local tools in `/workspace` and
scrubs Mango credentials from the shell environment. Bash is a persistent PTY
session within one Work container: working-directory and environment changes
survive later calls, `restart` creates a fresh shell, and `timeout_ms` overrides
the shell-call timeout without extending the runner-wide tool deadline. Timeout,
cancellation, shell termination, or
corrupt completion framing closes the old shell before another call can run.

Containers are removed after each activation. A Docker named volume derived
from the Session ID is retained, so later Work for the same Session resumes the
same workspace; shell process state deliberately does not survive that
container boundary. This reference does not yet prepare Skills, Memory, File/Git
resources, or Session outputs; it is not a hardened hostile multi-tenant
boundary. See the
[Docker worker deployment notes](https://github.com/yanpgwang/mango/tree/main/deployments/self-hosted/docker).

The file tools are confined to `/workspace`. Bash itself is intentionally not
path-confined within the container, so the Docker boundary and its mounts,
credentials, user, capabilities, resources, and network policy remain the
security boundary.

Workers must honor `evaluated_permission` independently of execution location.
An `ask` call waits for a persisted allow confirmation; a deny must never run.
After allow, execute and submit `user.tool_result` for the original call.
An approval alone does not clear the Session's pending-action barrier. On
reconnect, the Go runner reads confirmations and results as well as tool uses,
copies the owning Thread ID for child-call results, and exposes the original
event ID so a tool can make its external side effects idempotent. See
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

The supervisor uses a Workspace API key to Poll and Ack. Poll additionally
issues an unpredictable per-claim credential payload; only the SHA-256 digest
of its `sessions_token` is stored. That token is limited to the claimed Work's
Heartbeat and Stop, the claimed Session's read/event execution routes, and the
immutable File and Skill inputs pinned to that Session. On the event write
route it may submit only `user.tool_result` and `user.custom_tool_result`, not
ordinary user messages, interrupts, approvals, or `system.message`. It becomes
invalid when the Work stops, its lease expires, or it is reclaimed. An existing
Session event stream rechecks that ownership once per second and closes after
invalidation. A Workspace key retains full operator access and must stay in the
trusted supervisor rather than an untrusted Session sandbox. Mango does not yet
issue a narrower Environment-level polling key. A sandbox runner necessarily
receives its per-Work token, but tool subprocesses must not inherit that token
or other launcher credentials; use an explicit allowlisted environment or
scrub `MANGO_WORK_SECRET` before spawning them.

See [capabilities and limits](../capabilities.md) for the current support boundary.
