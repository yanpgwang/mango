---
title: Workflow program replay study
description: Measured JS and PostgreSQL protocol evidence, remaining feasibility gates, and no product capability claim.
slug: /design/workflow-runtime-study
---

# Workflow program replay study

**Status: first feasibility study, 2026-10-25. No product executor is shipped.**
This records evidence for the [program execution proposal](workflow-program-runtime.md).
The current Session/Thread/PostgreSQL/Temporal boundaries remain suitable; this
study does not establish the whole [pre-product gate](workflow-program-runtime.md#gate-before-a-product-implementation).

## Question and experiment

Can a disposable JS evaluator reconstruct sequential, parallel and
result-dependent Agent orchestration without repeating a committed invocation
or changing the order of callbacks after restart?

The independently authored fixture in
[`scripts/workflow-runtime`](https://github.com/yanpgwang/mango/tree/main/scripts/workflow-runtime)
compares QuickJS 2026-06-04 and Node 22.17.0. It runs generated fixture bodies in
separate restricted Linux containers and uses a real, isolated PostgreSQL
database for the ordered command/delivery trace, invocation intents, results,
epoch fencing and wait handoff. Agent outcomes are deterministic supplied JSON,
not model calls, product Thread creation or tool execution. No external SDK,
cookbook, hosted credentials or existing Mango deployment is executed.

Run `make test-workflow-runtime`. A path-filtered CI workflow runs the same
study for relevant changes. Ordinary tests exercise IPC validation and skip
the explicit Docker research tier. Test output records engine image and platform
identities, bounded replay timing and resource termination status.

## Observed protocol behavior

| Fixture | Evidence |
| --- | --- |
| Same Agent and prompt invoked twice | Two distinct invocation identities; replay creates no third intent. |
| B finishes before A; both callbacks invoke more Agents | The trace preserves B's delivery before A even when both are cached; dependent prompts and invocation order remain identical. `Promise.all` returns values in input order. |
| Acknowledgement lost after each of seven trace commits | Replace the actual evaluator after every command/delivery/return boundary of a specified three-invocation program; recover three intents and one committed final value. |
| Admission rejected, then live policy changes | Replay returns the saved rejection and enters the same catch/fallback path without retroactively admitting the old call. |
| One child rejects while its sibling runs | An explicit recovery call can proceed; the sibling remains pending and must settle before successful final return. |
| Rejection precedes a later await | Direct, `.then`-derived and async-wrapper Promises survive a live event-loop turn and evaluator replacement, then deliver the same caught failure. |
| Bounded result-driven loop and concurrency of one | Reconstruct loop variables from saved values; queued work waits instead of becoming a catchable capacity failure. |
| Epoch replaced, Run paused/stopped, receipt races wait | Stale writes are rejected; paused work may settle; terminal work cannot accept successful results; wait handoff preserves the receipt wakeup. |
| Wrong prompt at an existing trace position | Reject the mismatch before appending another invocation. This is deliberate evaluator/profile fault injection, not permission to edit admitted source. |

The SQL fixture deliberately omits Session authorization and product locking,
real outbox delivery, Temporal, provider output-schema validation and actual
worker cancellation. Its assertions do not establish those product properties.

## Engine findings and containment

Both research profiles use non-root containers, no network or host mounts,
read-only filesystems, dropped capabilities, no new privileges, bounded CPU,
memory, stack, PID count, IPC and output. Neither receives credentials or the
Docker socket. `node:vm` supplies a realm and microtask drain, not security.

Testing found three concrete wrapper problems and added regressions:

- An ordinary QuickJS memory exception could be caught to submit more work.
  The C fixture now latches allocator budget failure outside JS and checks it
  before exporting frames. Actual stack exhaustion uses a hard OS stack limit;
  the engine guard must not turn it into a recoverable branch. Node's engine
  stack setting is above the OS limit in this pinned study profile.
- Node's default unhandled-rejection policy terminated programs that would
  catch a derived Promise later, making result timing observable as process
  failure. The trusted host now leaves terminal success/failure to the returned
  program Promise. Tests cover live delayed handling and reconstruction.
- Freezing builtin objects did not freeze their global bindings. Lock the
  bindings as well so program reassignment cannot replace the driver's Promise,
  JSON or other used intrinsics.

Infinite loops, microtask storms and allocation/stack exhaustion terminate
within the fixture bounds, including catch/fallback attempts. Tests inspect
the container's actual nonzero exit status rather than accepting any pump
error as proof. Existing committed receipts survive exhaustion and no fallback
invocation is appended. A normal deterministic `RangeError` remains catchable.
The profile also rejects orphan waits, outstanding work at return, non-JSON or
oversized values, dynamic import and tested constructor escapes.

These probes are evidence for a pinned research profile. They are not proof
against arbitrary JS-engine/kernel vulnerabilities, all native builtins or
every platform's stack/optimization behavior. Do not turn fixture success into
a production safety or arbitrary-async-library guarantee.

## Bounded cost measurement

On local Docker Desktop/Linux arm64, one run with 64 invocations, 512-byte
inline results and one unsettled tail produced 129 trace entries (40,825 encoded
bytes). One observed sample, including Docker startup, SQL and hydration:

| Candidate | Initial pump | Cached-prefix replay |
| --- | --- | --- |
| QuickJS | 329 ms | 261 ms |
| Node | 310 ms | 255 ms |

These are individual feasibility samples, not a benchmark or engine ranking.
They do not establish supported production scale, worst-case replay cost,
cross-platform trace equality or an automatic checkpoint optimization. The
source remains reconstructed from the start and repeated wakeups can incur
quadratic work. Test logs contain the actual image identity for each run;
source/base versions and the QuickJS checksum are pinned in the research build.

## Existing coordinator and Thread recovery experiment

A separate real-service test, `TestWorkflowThreadRecoveryRealTemporal` in
`internal/temporal/workflow_thread_recovery_integration_test.go`, exercises the
current Mango implementation. It uses migrated PostgreSQL, the real Temporal
service, `SessionWorkflow`, `SessionThreadWorkflow`, `Activities`, the tool
journal, completion transactions, `Signaler` and `Relay`. Only model responses
and transport fault boundaries are simulated. There are no alternative Run
tables, scheduler or executor in this experiment.

| Injected boundary | Assertion on current Mango components |
| --- | --- |
| Signal accepted but acknowledgement lost | The relay retains the real outbox row and redelivers it; PostgreSQL consumption repairs the duplicate signal. |
| Delegation committed but Activity reply lost | A replacement worker retries the same Activity ID and logical turn/tool-step input, with an increased Activity attempt number. Two identical Agent calls still create exactly two child Threads and two journal steps. |
| Primary progress message while children are active | The primary can answer and Continue-As-New without repeating delegation; aggregate final end-turn remains blocked. |
| Child completion cannot insert its primary wakeup | A temporary constraint in the isolated test schema forces transaction failure. The child report and trigger consumption both roll back. Removing the fault permits the same Activity to retry. |
| Child completion committed but Activity reply lost | The same completion input retries without duplicating its report, transcript or usage. The report, processed trigger and pending outbox row survive together. |
| Reports cross Continue-As-New and worker replacement | Actual low-threshold Workflow executions continue with new Temporal Run IDs. Sticky cache is purged between workers; the primary receives both reports and emits its final idle after them. |
| Duplicate, wrong-owner or archived-owner completion | A duplicate replays committed state; another Thread's trigger conflicts; a late completion cannot reopen an archived Thread or append output. |

Run it against isolated services with `MANGO_TEST_DATABASE_URL` and
`MANGO_TEST_TEMPORAL_HOSTPORT` set:

```sh
go test ./internal/temporal -run '^TestWorkflowThreadRecoveryRealTemporal$' -count=1
```

The normal `make test-service-core` tier includes this test. Default offline
tests skip it when service addresses are absent; explicit addresses must fail
if either service is unreachable. Its database schema, task queue and Workflow
IDs are unique, and cleanup terminates only its own Workflows and schema.

This establishes recovery properties of the existing ordinary child-report
path. It does **not** establish a Run pump request/decision identity across
Continue-As-New, a Run-owned invocation/attempt/result contract, evaluator epoch
fencing, structured result validation, or the open-Run/Outcome barrier. In
particular, the existing report goes to the primary; a future Run-owned result
must settle its Run instead. Worker objects are replaced in one test process;
no database/Temporal server crash or host-process kill is simulated.

## Remaining decisions before product exposure

Keep the gate open. Beyond the existing-component experiment, integration must prove the trusted Temporal pump
request/decision identity, retry after SQL acknowledgement loss,
Continue-As-New and reconciliation against actual product ownership and outbox
transitions. An evaluator crash is not evidence that no command committed.

Select the deployable engine/profile after verifying runtime availability,
upgrade/platform pins, installation on local/Kubernetes operators and hard
resource behavior there. Define cumulative replay/quantum accounting and the
terminal classification/recovery of abrupt evaluator exits; an unbounded retry
of a resource-exhausting program is not an acceptable implementation.

Then implement the owned invocation result adapter and a supplied-program
vertical slice on existing Threads, including Session completion/Outcome,
budget/approval/interrupt, two Runs plus the primary, late usage and scoped
cleanup. HTTP/OpenAPI and all three SDKs must move with that product slice.
The public program wrapper/helper spelling, schema-error contract and operator
controls remain proposals until those independent contract tests exist.
