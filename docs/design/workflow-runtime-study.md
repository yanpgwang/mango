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

## Internal owned-invocation receipt kernel

The next internal slice adds storage and completion ownership, not an enabled
Workflow product. `internal/pg/workflow_receipts.go` has trusted Go admission
operations; no HTTP handler, model tool, CLI command, feature setting or scheduler
calls them. Source/input/profile bytes are frozen as identity data and are not
evaluated or certified. The normal relay does not consume the new Run outbox.

The additive `20261025000001_workflow_receipts.sql` migration stores Run admission
identity and frozen Session roster, Run-local invocations, first business attempts
with unique owning Threads, immutable terminal receipts and versioned Run wakeups.
It does not reuse `turn_attempts` as business attempts or require a fabricated
primary tool call. Transport replay reuses the invocation/Thread; explicit
business retry on a new Thread is not implemented.

The actual Thread Workflow now passes its terminal model response's event ID and
disposition as private completion metadata. The bounded text adapter accepts only
that response; it cannot fall back to an earlier tool-round message. Unsupported,
missing or oversized output becomes a typed failure. The initial format is text;
JSON/schema validation remains a separate gate. Closed/deadline-ended ownership
cannot accept a successful business value. Actual transcripts and usage remain
retained. Approval/budget waits do not settle a result.

Under Session-first locking, owned completion commits its receipt and Run wake
with the existing Thread events, transcript, consumption and usage transaction.
It suppresses the ordinary per-child primary report. Ordinary follow-up and
archive operations cannot bypass the ownership record. The new PostgreSQL tests
cover identity/conflict/concurrent replay, wrong-owner/stale returns, frozen
roster and tenant scope, bounds/deadline, and rollback after wake/spawn failure.
`TestWorkflowOwnedReceiptsUseExistingThreadRunner` uses real PostgreSQL, Temporal,
the existing worker/Thread loop and relay, with deterministic model responses
and post-commit ACK loss. No evaluator language or alternate agent loop is used.

**This kernel must remain unreachable from product admission.** The frontier
gate below adds epoch-fenced database decisions and a grading exclusion boundary.
It has no Temporal Run pump, terminal Run delivery, physical cancellation,
open-Run aggregate/retirement barriers or complete Session/Run stop lifecycle.
Tests may create these internal rows; supported applications cannot. Migrating
the tables does not enable a Workflow capability. Before adding an entry point,
implement those barriers and jointly review the HTTP/OpenAPI/SDK contract.

The receipt-only slice leaves Outcome-versus-Run admission open. The next gate
implements Session-serialized mutual exclusion for actual external grading,
while Outcome working/revision stages can still start Runs. Durable parking,
resumption and fresh synthesis after Run results remain product gates.

## Internal fenced frontier gate

The additive `20261026000001_workflow_frontier.sql` migration and
`internal/pg/workflow_frontier.go` establish a one-command database pump boundary.
There is no evaluator, Temporal Run coordinator, dispatcher or public caller.
Application-owned claim/request identities survive Activity replacement and
Continue-As-New; this slice tests those identities at the PostgreSQL boundary,
without claiming a new Temporal pump integration test.

Under Session-first then Run locking, evaluator claims replace ownership by an
epoch compare-and-swap. A request checks the current epoch, expected journal
cursor, Session/Run authority, absolute deadline, independent pause reasons and
shared budget. It commits the immutable command/decision with the actual existing
invocation, Thread input and Thread/Run outboxes in one transaction. A retry by
the current owner returns the same decision; an old epoch cannot progress even
through a cached decision. Once a Run has been claimed, the older trusted
unfenced admission helper refuses writes. An existing unfenced effect prefix
cannot be adopted without reconciliation.

Semantic argument/unknown-Agent/unsupported-command rejections consume journal
positions and are durably replayable. Control waits, revoked authority, changed
traces and protocol-limit failures return host errors, without a script-visible
catchable decision. A future adapter must preserve this distinction. Fixed
internal bounds are 256 decisions and 8 MiB of charged command/decision JSON;
replay does not charge again. These bounds do not replace pre-parse IPC limits,
evaluator resource containment, ordered Promise delivery or cumulative execution
budgets. Full schema/adapter/engine/image/platform pins remain required before
an executor can use this boundary.

Global interrupt records its independent Run pause under the Session admission
lock, including the budget-idle eventless control path. Clearing another reason
or removing budget does not clear that pause or extend the deadline. Existing
Thread execution/usage behavior is retained. Child admission now also queues
self-hosted Environment Work atomically, so an earlier worker retirement cannot
strand its new tool execution. The aggregate completion and retirement barrier
for an open Run is still missing.

The production Outcome Activity reserves its already persisted primary grading
start before provider I/O. The reservation binds Session, Thread, triggering
Outcome turn, Outcome/iteration and expected start/end identities. New Run
admission and grading reservation serialize on the same Session lock: the winner
excludes the other. An active Outcome's working/revision stage remains eligible
to admit a Run. A completed grading prefix can release its exact reservation
before the Session Outcome projection advances to the next iteration.

Reservation release/fencing commits with the owning end event in the progress
or final-completion transaction, including correlated interrupt/failure ends.
When a completed `needs_revision` grade races an interrupt, its end remains in
the committed prefix and releases that reservation; the interrupted revision
has no grading start. A distinct next open grading span retains its identity.
It is never released in an Activity defer or by a timeout alone. Missing
historical/direct start ownership fails closed in the production adapter.
Open-Run denial is an Activity/control error before inference, not a failed
business verdict. No durable Run-aware grading parking/resume loop is provided
yet. Forced termination or an orphaned reservation still requires explicit
lifecycle reconciliation; silently expiring the reservation could let an old
grader race new work. Known usage accounting remains separate and unchanged;
this does not promise exactly-once provider execution or known usage for a lost
response.

Real PostgreSQL tests cover ACK replay, replacement epochs, competing cursors,
durable rejections, control/byte bounds and transactional rollback after child
creation. They invert the lifecycle research's grading-admission, independent
interrupt and successor-Work counterexamples. The actual Activity adapter test
proves reservation failure prevents provider I/O and is not a business verdict.
Existing Outcome, ordinary Thread, receipt, interrupt and retirement tests remain
regression gates. No public HTTP/OpenAPI/SDK contract changes here.

## Internal Run coordinator gate

The next internal boundary connects the existing frontier and owned receipts to
an opt-in Temporal `WorkflowRunWorkflow`, Activity and versioned Run outbox relay.
The default worker, application startup and ordinary Thread relay do not enable
it. There is no shipped evaluator implementation, public admission or new journal.
The integration evaluator is a test-only sequential protocol, not a workflow
language or a qualified replay adapter.

Application pump/claim identities are derived from the journal cursor, not the
Temporal Run ID or Activity attempt. Claim ACK loss reuses the claim; a committed
decision is recovered before evaluator work. Advancing the cursor acquires the
next epoch and fences an old Activity. Source, prompts, the journal and current
owned receipts stay inside an Activity; Temporal carries bounded identifiers,
cursor, previous epoch and a wakeup version hint across Continue-As-New.

Signals are hints. The coordinator drains accepted signals before waiting or
continuing, compares their versions with its database snapshot, and periodically
reconciles PostgreSQL even without delivery. Relay deletion uses a version and lease-token CAS after Signal-With-Start
acceptance. `REJECT_DUPLICATE` protects closed IDs only while Temporal retains
history. PostgreSQL now permanently binds the first coordinator chain and its
current physical execution. Once bound, a fresh chain cannot adopt the same Run, even at
cursor zero. Only a same-chain Continue-As-New predecessor with the persisted
cursor/epoch checkpoint may transfer ownership; a waiting checkpoint must match
its durable `pump/N` claim. Every claim, snapshot and command checks this physical
owner before returning cached data. Physical IDs are infrastructure fences;
Run identity and `pump/N` business identity remain unchanged.

Before its first remote start, the relay separately reserves a durable
`coordinator_start_attempted` flag under Session/Run/outbox locks and verifies the
current lease/version. Only that reservation's first response permits
Signal-With-Start. All later deliveries use Signal only, including after a
higher version or replacement outbox row. This prevents automatic recreation
even when termination precedes the first Pump/binding. A claim ACK loss before
preparation only expires a lease; a preparation ACK loss or crash before RPC
consumes the start allowance and requires explicit reconciliation. There is no
automatic second start. Before a chain is bound, PG cannot identify an arbitrary
manual Temporal start; such starts are unsupported. This is a failclosed relay
contract, not a new public recovery API.

The Workflow rejects Reset, Workflow retries and Cron metadata. The Activity
also checks the actual SDK execution ID, preventing a replayed old Activity
input from authorizing a new physical execution. Unsupported Reset can fail a
replay task through nondeterminism or fail the execution through a nonretryable
fence; it is not a supported business rollback. External termination does not
undo admitted Thread work, recorded usage or external effects. Its PG chain
binding remains and prevents a fresh logical restart.

Independent PostgreSQL restore remains unsupported. Stop all coordinator/relay
workers and keep this opt-in registration disabled during restore; do not clear
the chain binding or restart at cursor zero. Reconcile PG decisions/receipts with
Temporal histories and external effects before defining an explicit recovery
generation. A whole-database rollback can remove the binding itself and cannot
be detected using that rolled-back database alone. This slice supplies no
automatic restore/resume route and makes no exactly-once claim for such effects.

The acceptance test uses actual PostgreSQL, Temporal, Run/Thread outboxes and
the existing Thread runner. An independent deterministic model oracle checks two
sequential prompts and exact terminal answers. Fault schedules cover claim
and decision ACK loss, worker replacement, stale evaluator rejection, a signal
accepted before wait, a withheld wake recovered by reconciliation, and real
Continue-As-New without repeated invocation or inference.

The withheld-hint schedule retains the real outbox row; it neither erases a wake
nor invents a successful signal. Worker replacement stops one SDK worker, not
PostgreSQL, Temporal or the host. Coordinator histories are checked for bounded
reference payloads and absence of program/prompt/receipt markers. Raw error
messages, causes and details are sanitized before Temporal failure conversion;
a sentinel test checks that boundary while retaining retry classification.

The relay reserves due outbox rows with `FOR UPDATE OF wake SKIP LOCKED`,
without taking Session locks after outbox locks. It claims one delivery at a
time, uses PostgreSQL time for eligibility/leases/backoff, persists the attempt
and a 30-second lease before IO, and limits delivery
to 10 seconds. Failures defer with exponential delay from one second to a
one-minute cap. Eight attempts, including abandoned leases, quarantine the
durable row instead of consuming every scan forever. A higher Run version
re-arms retries and invalidates the lease; equal versions cannot reset them.
Only the current version/token can ACK or defer. Session deletion cascades these
rows. Signal acceptance ACKs delivery, not coordinator binding: after absent history,
a fresh Temporal chain can accept the hint and the relay can delete its wake
before the Pump rejects PG ownership. Reconciliation must inspect the persisted
Run binding/start reservation, not assume every rejected chain retains an outbox row.
Quarantined rows require explicit operator reconciliation; no product
re-arm endpoint, metrics or automatic recovery policy is shipped.

`TestWorkflowRunOwnershipAfterAbsentHistoryAndResetRealTemporal` tests completed
and terminated cursor-zero coordinators against fresh physical chains for the
same PG business Run. Its fixture uses a fresh Temporal ID to model an empty
deduplication slot; it does **not** wait out retention or claim to delete history.
The actual Reset probe resets after a completed Pump Activity and checks the
worker's nonretryable fence or nondeterminism, excluding Temporal's automatic
Reset marker. Independent PG/evaluator counters remain unchanged. A real relay
with 100 intentionally blocked targets still delivers the later healthy wake to
the actual coordinator. PG tests cover concurrent chain/lease claims, valid and
stale CAN ownership, backoff deadlines, abandoned leases, retry exhaustion,
version/lease CAS, one-time concurrent start reservation and deletion cleanup.
Actual service fixtures also terminate an unpolled coordinator before any Pump
and simulate preparation ACK loss/crash before RPC. Later wakes cannot start a
new coordinator and retain their retry row for reconciliation.

The test evaluator waits at its final checkpoint. This does not close the Run,
deliver a terminal Run event, resume Outcome grading, or certify aggregate
completion/retirement. Ownership, lifecycle and protocol errors fail the private
coordinator closed rather than selecting a new product pause/stop policy.
Infrastructure retries keep their existing pump identity. Public admission must
stay unreachable until lifecycle reconciliation, ordered Promise delivery, full
runtime/adapter pins, terminal delivery, grading resumption and containment gates
are complete. The snapshot's command order is not Promise completion order.

## Remaining decisions before product exposure

Keep the product gate open. The internal coordinator test establishes a bounded
sequential recovery path over the existing PostgreSQL decisions and owned
receipts. It does not qualify ordered program replay, automatic coordinator recovery after closure or independent restore, control
parking/resumption or complete Run lifecycle.
An evaluator crash is not evidence that no command committed.

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
