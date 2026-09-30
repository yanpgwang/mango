---
title: Environment execution healthchecks
description: A bounded, durable check of self-hosted worker execution without a Session or model request.
---

# Environment execution healthchecks

An operator needs to prove that an Environment's supervisor can claim and
acknowledge Work, launch a sandbox, execute a process, and return a durable
result before sending a real Session to it. Queue statistics show polling but
cannot establish that execution works.

## Contract and invariants

`POST /v1/environments/{environment_id}/work` with
`{"data":{"type":"healthcheck"}}` creates a Work item with HTTP 201. Only
Workspace credentials may create it. Creation follows ordinary resource
creation semantics; a repeated POST creates another check. No Session, Agent,
model request, event stream, File, or persistent Session workspace is created.

Work data is a union: `session` carries a Session `id`; `healthcheck` carries
only its type. Existing queue, claim, acknowledgement, heartbeat, lease expiry,
token rotation, and stopped state are reused. A healthcheck has `expires_at`
fixed at creation plus 120 seconds and nullable `result`. Results have a status
of `succeeded`, `failed`, `timed_out`, or `cancelled` and a bounded message.
Session Work has null expiry and result.

The reference Docker launcher uses the normal image, uid, resource limits,
read-only root, network settings, stdin credential delivery, and cleanup. A
healthcheck uses an ephemeral tmpfs workspace instead of a Session volume or
operator bind mount. Its fixed Bash subprocess writes, reads, and removes a
small file, with no caller-provided program or external-service probe. Execution
is limited to 10 seconds; the launcher also bounds container lifetime and
forcibly removes a stalled process. Docker reconciliation and cleanup use
separate 15-second API request bounds; an unreachable daemon can prevent
confirmed removal and is reported as a launcher failure. No secret is delivered
after attempt cancellation, including a late Start response. The helper's provider callback receives
only a bounded context and local work directory; the reference callback refuses
to execute without the launcher's sandbox marker.

`POST .../work/{work_id}/result` commits `succeeded` or `failed` and transitions
to `stopped` atomically. Only the current per-Work credential may report a
result. The database checks its digest and live lease in the same transaction.
Identical retries are accepted; conflicting results are rejected. A completed
check's credential remains valid for 30 seconds solely for retrying that result
only. It cannot read Work, heartbeat, claim, or access Sessions. An
expired/reclaimed claim cannot complete a successor's check.

Lost starting/active leases can be reclaimed under the same Work ID before the
120-second deadline. Overdue checks become `stopped` with `timed_out` when Get,
List, Poll, Ack, Heartbeat, Result, Stop, or Stats reconciles that Environment.
There is no background timer guarantee when no requests arrive. Stop records
`cancelled` and prevents further execution or result changes. Terminal results
remain durable until their Environment is deleted. Reads never disclose secrets.

## Acceptance and non-goals

- Raw HTTP and native SDK clients can create and inspect a healthcheck.
- PostgreSQL tests prove no synthetic Session, durable completion, concurrent
  claim ownership, lease reclaim, deadline expiry, immutable results, retry
  behavior, cancellation, and token confinement.
- A Docker test proves supervisor → claim/Ack → fixed sandbox execution → result
  and checks container/ephemeral-workspace cleanup without model credentials.
- Existing Session Work and SDK conformance continue to pass.

This does not diagnose providers, add startup endpoints, schedule periodic
checks, accept arbitrary host commands, introduce broad IAM, or certify hostile
multi-tenant isolation. A successful check proves the configured worker path
at that instant, not every later Session input or tool.

## Research and migration

Reviewed current official [self-hosted sandbox documentation](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
and [Work API](https://platform.claude.com/docs/en/api/beta/environments/work)
on 2026-09-29 alongside the optional official checkouts. The GitHub release
metadata confirmed these as the latest published core SDK releases on that date
(all published 2026-09-28; TypeScript is the `sdk-v` package family):

| Reference | Reviewed revision |
| --- | --- |
| Go SDK v1.76.0 | `ad865dfa3d1a8d2f4a7ad0d072011e811e9957a9` |
| Python SDK v1.9.0 | `a7285e919ab79998d9380b3b57f6315b7860b8d8` |
| TypeScript SDK sdk-v0.129.0 | `bf2058689f845dfb10e59bd9ebeb5cb4e9318a9d` |
| managed_agents/self_hosted_sandboxes cookbook | `d7265d6ae994ccd8429db0594b000073b2f9ad43` |

Adopted the healthcheck variant inside the existing Environment Work hierarchy
and the supervisor/per-item helper division. The reviewed public worker helpers
skip non-Session items; Mango instead executes a fixed local check and records
its result because self-hosted operators need observable execution evidence.
Mango omits the separate opaque healthcheck identifier, hosted connectivity
probes, beta headers, Console-only management, and hosted credentials. Official
code is research material only, never executed or added as a dependency.

The schema permits null Session/activation fields only for healthchecks and
includes type, deadline, and result storage. The original migration 41 and its
downgrade path were consolidated into the
[development schema baseline](../deployment.md#development-database-baseline).
Development databases from the historical chain must be rebuilt.

## Implementation plan

Use test-first changes in these three reviewable units:

1. Add raw HTTP and PostgreSQL regression tests for creation, scoped results,
   reclaim, expiry and retry; then implement domain/app/HTTP/repository and
   schema constraints. Verify with the targeted HTTP and PostgreSQL commands.
2. Update OpenAPI's Work data union and create/result operations, regenerate
   all SDKs, then add worker tests for callback timeout and token isolation.
   Implement the Go worker callback and Docker's ephemeral bounded path.
3. Add the real Docker execution test and native SDK contract coverage, update
   operator/capability/provenance docs, run required package, race, SDK, service,
   documentation and configured live checks, and obtain independent review.

Review focuses on a deadline crossing a result transaction, a lost success
response, a reclaimed stale token, a cancelled sandbox, and launch failure before
the first heartbeat. Each must preserve a truthful durable result or a bounded
reclaimable item, never a fabricated success.
