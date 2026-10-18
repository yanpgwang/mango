---
title: Workflow program execution and recovery
description: Proposed restricted JavaScript execution, durable command replay, and context boundaries.
slug: /design/workflow-program-runtime
---

# Workflow program execution and recovery

**Status: design proposal, 2026-10-19; nothing on this page is implemented.**
This is the execution detail of the [Workflow Runs plan](workflow-runs.md).
The recommendation is restricted JavaScript for orchestration, a PostgreSQL
command journal for semantic recovery, and Temporal for durable coordination.
Engine selection and the protocol feasibility gate remain open. This page is
not an API specification, an executable prototype, or evidence of isolation.

## Execution boundary

The program builds prompts, invokes approved Agents, processes their results,
and chooses subsequent work. Ordinary JavaScript handles arrays, conditions
and loops; Mango should not need a new JSON node type for every control-flow
pattern. JSON remains the representation of inputs, results and host commands.

Use one trusted Go Temporal Workflow per Run. It schedules bounded evaluation
Activities, waits on durable wakeups/deadlines and continues as new when needed.
An Activity's trusted host adapter loads authorized records and starts an
isolated, disposable JS evaluator. Generated JS never executes in the API, Go
orchestrator process, Temporal Workflow callback, or a `workflow.SideEffect`.
Moving JS into an Activity alone does not make it safe or resumable.

This adds a semantic replay journal alongside Temporal history, so its complexity
must earn its place. Running the generated program as a Temporal TypeScript
Workflow could reuse Temporal's command history, but would introduce a different
SDK/worker and need a proven untrusted-code, resource and upgrade boundary.
Embedding an interpreter in a trusted Go Workflow conflicts with the selected
containment boundary; a long-lived JS Activity without semantic replay loses
control state on retry. The isolated Activity/PG proposal fits Mango's current
Go orchestration and separates untrusted code, at the cost of implementing and
testing its own ordered program trace. If that protocol is too costly or cannot
pass the gate, reconsider this architecture before building it; do not assume
two replay logs are automatically the simplest choice.

The evaluator is a separate operator-controlled runtime capability. It is not
the Session's Environment Work, a tool container, an Agent Thread, or a hosted
agent service. It receives program/input bytes and a narrow framed IPC channel;
it receives no database/Vault credentials, Workspace API key, Docker socket,
Session filesystem mount or network authority. Its host adapter owns database
access. Local and Kubernetes distribution need an explicit restricted runner
profile; there is no fallback to evaluating JS in a privileged process when
that profile is unavailable. Agent tools continue on existing operator workers.

The process/container boundary must limit memory, CPU, stack, process count,
IPC frame size and output. Engine resource hooks supplement OS isolation.
[Node's VM documentation](https://nodejs.org/api/vm.html) explicitly excludes
untrusted-code isolation from `node:vm`'s guarantees.
[QuickJS](https://bellard.org/quickjs/quickjs.html) offers memory/stack limits and
interrupt hooks, but those do not establish containment or deterministic replay.
Compare an isolated QuickJS-based evaluator with a separately confined V8/Node
evaluator at the first gate. Neither engine nor packaging is selected here.

## Proposed program surface

Keep one pinned language/host-API profile for each admitted Run. The target
certified profile should support predefined-Agent invocation, sequential `await`,
bounded fanout/join, JSON result validation, phases, pure result transformations,
and result-driven `if`/loops. The first journey can be small without introducing
a temporary public DAG language. Inline Agent definitions and pipeline helpers
can follow through the same invocation boundary.

| Construct | Proposed meaning |
| --- | --- |
| Immutable metadata and `args` | Bounded name/description/phase declarations and JSON inputs; the exact module/function wrapper is settled with the engine gate. Metadata cannot execute code during admission. |
| `agent(prompt, options)` | Request one predefined Agent invocation under Run authority; synchronously establish invocation order, return a host-managed Promise, and resolve only with a committed validated result. Method spelling/options remain proposed. |
| `parallel(...)` | Frozen ordered membership, bounded admission, ordered aggregate results. The pinned helper defines failure handling and uses the journaled scheduler. Eager Promise creation cannot bypass Run/Session concurrency limits. |
| `phase(...)` | Journaled progress metadata, not a dependency, permission grant or task identity. Phase and diagnostic replay must not duplicate public events. |
| `return value` | A bounded JSON/text Run result, accepted only when all admitted invocations are settled; a returned program with outstanding work is an error followed by scoped cleanup. |

On successful `agent()` completion, return a deep-copied JSON value or bounded
text, according to its declared output contract. A definite terminal child
failure rejects with a documented serializable error; it is not an empty value
that looks successful. A script may catch it and explicitly collect a partial
result. An uncaught failure fails the Run and fences/cancels its remaining work.
`parallel`'s proposed default is the familiar ordered `Promise.all` behavior;
catching its rejection does not itself cancel sibling invocations. A collected
failure mode needs explicit envelopes. Confirm this surface before release.

A new call chosen by a script loop is new business work, even when its prompt
matches an earlier call. The host cannot infer business idempotency from text.
Trace replay deduplicates the same invocation, not semantically similar actions;
unknown tool effects still need the existing journal, idempotent tool behavior
or an explicit reconciliation decision before retrying that business operation.

### Invocation return and validation

Today's ordinary child reporting uses `lastAgentMessageContent` in
`internal/pg/thread_completion.go`. It is not a program-result contract. The
current Messages adapter's `output_config` carries effort, not a schema output
format. Workflow implementation therefore needs an explicit return adapter;
do not assume an `agent(..., {schema: ...})` option is already executable.

The proposed first contract selects bounded text or JSON-schema output at
invocation admission. Only the expected attempt's normally completed turn,
with no unresolved actions, can produce a successful candidate. For text,
extract ordered text blocks of that terminal response under a fixed rule;
for structured output, parse the entire resulting text as JSON and validate
it against the admitted schema. Do not parse a preview, another turn's message,
Markdown fences or a convenient JSON substring. Refusal, interruption, missing
output, unsupported content and schema/size failure settle explicitly rather
than masquerading as successful values.

The trusted adapter owns bounded parsing/validation, with no remote schema
references. Optional provider-native schema enforcement may improve output
quality but cannot replace this check or become a required model integration.
Persist the adapter/instruction/schema revision and validated value/failure
with the owned completion before any JS delivery. Validation failure initially
fails that invocation; a repair model request is separately admitted/accounted,
never hidden in parsing or replay. Normal provider continuation/transcripts and
actual usage remain retained even when the program result is invalid.

Run-owned completion must bypass the ordinary per-child primary report and
atomically settle its invocation receipt plus Run wakeup. This result path
needs its own PG/Temporal tests; an idle Thread alone cannot settle it.

Pure JS can use native control flow, but the certified profile controls Promise
combinators and job scheduling. Do not claim arbitrary async libraries work.
Initially defer `Promise.race`/`any`, timers and externally resolved Promises;
either remove unsupported capabilities or reject their use, with deterministic
errors. The gate must prove the supported `parallel`/Promise semantics, including
microtasks, early rejection and pipelines added later.

There is no `fetch`, filesystem/shell, `process`, ambient environment, module
loader, dynamic import, native extension or direct model client in this realm.
Disable dynamic code generation and unavailable capabilities at the runtime
boundary; source substring checks are insufficient. Untrusted tool results
remain data and are never evaluated as new code. Time/randomness/locale-sensitive
behavior must not be ambient: provide a frozen timestamp or seed in `args` when
needed and forbid uncertified APIs. A wall-clock timeout stops runaway code; it
must not become a catchable JS branch that changes the replay trace.

Pin the source bytes/hash, wrapper/compiler, engine build, host/helper profile,
JSON encoding rules and required execution platform. A multi-architecture image
index alone does not pin identical floating-point/library behavior. Verify
cross-platform trace equality or schedule a Run only on its pinned platform.
No silent engine/helper upgrade is permitted while a Run is open.

## What PostgreSQL saves

These are logical records, not approved SQL tables or public field names:

| Record | Durable facts |
| --- | --- |
| Program | Exact bounded UTF-8 source and digest, inert metadata, language/engine/helper profile and platform identity. Initial source can live in PostgreSQL; compiled bytecode is a disposable cache. |
| Run admission | Session/Workspace owner, program revision, canonical input values/references, predefined Agent snapshots, authority ceiling, limits, absolute deadline and retry-command identity. |
| Program journal | Monotonic Run-local ordered host calls and their success/typed-error outcomes, result deliveries, group closure, progress and final-return decisions. Store immutable arguments or immutable references as well as fingerprints; hashes alone cannot rebuild prompts. |
| Invocation and attempt | Run-local invocation identity, effective Agent definition, exact prompt/output schema, owning Thread, explicit business attempt identity and terminal state. |
| Result receipt | Validated value or typed failure, schema/encoding revision, originating Thread/events, usage correlation and retention ownership. Result identity survives evaluator restarts. |
| Evaluation ownership | Fencing epoch/lease, expected journal position, bounded pump request/decision identity, wait set and next wakeup intent. |
| Terminal delivery | One Run result and durable primary-consumption identity, plus cleanup intents. |

Agent names, labels, prompts, source lines and Thread IDs are not invocation
identity. Repeated identical calls and loop iterations are distinct invocations
in the recorded call order; transport retries refer to the same invocation.
A new business attempt has a separate identity and retains the previous result.

Every host outcome visible to JS must be durable, including a catchable
argument/schema/authority rejection before any child exists. A rejected host
call retains its ordered command identity and typed error, without a spawned
Thread; replay returns that error rather than retrying admission under changed
policy. Otherwise a script's catch/fallback branch can change on restart.
Infrastructure failures before a decision commits remain host/pump retries,
not speculative JS rejections. Temporary concurrency/capability waits, budget,
approval, interrupt, stop and deadline controls are authoritative host states,
not catchable exceptions through which a script can bypass limits. Count rejected
host calls against the bounded command budget; replay/wait delivery cannot
charge the same logical call again.

Use Session-first locking followed by Run/owned records, uniqueness constraints
and expected-version/epoch checks. Commit commands, owned Thread/admission
intents, events and outbox atomically before dispatch. No interpreter, model,
object-store or worker I/O occurs inside SQL transactions. Run-specific wakeups
must extend the existing outbox rather than pretending today's Thread-only
cursor already provides Run command deduplication.

## Two recovery mechanisms with distinct owners

Temporal replays the trusted Go coordinator's Activity commands and timers.
PostgreSQL replay reconstructs the generated program's semantic state. Neither
log is a replacement for the other:

- Temporal history/Continue-As-New carries Run IDs, journal/version cursors and
  bounded decision references, not program source, prompts or batch results.
- Evaluation Activities may retry after committing SQL but losing their reply.
  Reuse the bounded pump request identity and return its committed decision when
  present. Never infer that an Activity timeout means no commands were admitted.
- The PG journal authorizes Agent invocations. Temporal signals and NATS messages
  are wakeup hints; duplicate or lost delivery is reconciled from durable state.
- A journal version or command ID is not an externally executable permission.
  The trusted host rechecks Run state, authority, limits and ownership.

Do not serialize a JS heap, Promise, stack or closure to PostgreSQL in the first
design. Reconstruct variables by rerunning frozen code against frozen inputs and
recorded host outcomes. Pure JS runs again; committed Agent work is looked up.
This is a proposal to prove, not a property supplied by Temporal automatically.

### Ordering is part of the persisted state

Saving only `call -> result` is insufficient for asynchronous replay. Returning
all cached results as immediately resolved Promises can change callback order,
downstream prompts and the order of subsequently issued calls.

For example, one possible committed trace is:

```text
invoke A
invoke B
deliver B's committed result
invoke C using B
deliver A's committed result
deliver C's committed result
commit final return
```

The evaluator must reconstruct that trace, including the recorded delivery
order, even if all three results are already available on restart. It must not
deliver A while replay is still expecting the invocation of B. Define explicit
scheduler turns: run the JS job/microtask queue to a bounded quiescent point,
commit the next eligible result-delivery decision, then inject it. Promise
resolution/rejection consumes that durable delivery record before callbacks
continue. A child result may be committed without yet being delivered to JS.

Compare every replayed invocation's ordinal, kind and canonical arguments with
the journal prefix. Reuse matching invocations/receipts; a mismatch stops new
effects and produces a diagnostic replay failure. Do not match by prompt hash
alone, silently discard a mismatching suffix, or launch replacement children.
Only after validating the existing prefix may execution append new commands.
The host validates all IPC even if the evaluator claims it finished replay.

Replay returns recorded successes and catchable failures without rerunning old
admission decisions. Current policy, pause/deadline checks and authority govern
new frontier effects; replay cannot redispatch a cached call to evade revocation.
The host still enforces current lifecycle fencing before progression or delivery.

Pure branch selection need not have a database row for every `if`: identical
inputs and delivery history must reproduce it. Any resulting host command,
phase transition, group closure or final return is checked against the journal.
This replaces the earlier idea of persisting a separate declarative branch
node for each control-flow choice.

### Bounded evaluation pump

1. A committed start/result/control transition enqueues a Run wakeup. The Go
   coordinator requests a bounded evaluation pump by stable request identity.
2. The Activity claims/reconciles the PG evaluation lease and fencing epoch,
   reads immutable program/input/journal references, and starts a fresh isolated
   evaluator. A retry first checks the prior pump's committed decision.
3. Replay the existing trace. Every host message is checked against committed
   identity; recorded results are delivered only at their recorded scheduler
   turn. All DB access stays in the trusted adapter.
4. At the current frontier, the adapter admits validated commands or deliveries
   through short transactions. It acknowledges them to JS only after commit.
   Replayed commands do not consume a second logical quota or create new work.
5. When the program is waiting for unsettled invocations, finish the pump with
   a durable wait-set/version reference and destroy the evaluator. Coalesce
   available results into bounded batches; no Activity or evaluator waits for
   an entire hours-long Agent task.
   Quiescence without a supported durable wait source is a program error, not
   an indefinitely parked Run awaiting an unregistered Promise.
6. Commit the final Run result, or a failure/stop decision and cleanup intents,
   under the same lifecycle lock rules. Destroying an evaluator alone never
   completes or cancels its Agent invocations.

The wait handoff must close the lost-wakeup race. Compare the observed journal
version/result inbox while committing the wait; if a receipt/control transition
arrived meanwhile, retain/re-enqueue a wake. A duplicate wake is harmless and a
periodic reconciliation path must repair a missing Run coordinator or relay.

At most one epoch can append a Run's trace. Activity cancellation, heartbeat
loss or lease expiry may leave an old evaluator briefly alive; all its later
commands, deliveries and terminal writes must fail fencing checks. A retry
reconciles already committed commands rather than assuming epoch replacement
undoes them. A paused/stopped/deadline-ended Run rejects fresh effects even from
the current epoch. While a Run is only paused, already admitted child work may
settle a durable receipt under its attempt identity; JS delivery/progression
waits for the relevant pauses to clear. Once the Run is terminal, late arrivals
may retain audit, actual usage and effect-reconciliation evidence, but cannot
accept a successful business result, satisfy a join, or reopen a pending barrier.

Replaying the prefix has CPU and hydration cost, potentially quadratic over
many wakeups. Set source, command, result-byte, replay-work and pump limits;
measure the representative fanout before choosing supported limits. Microtask
storms, pure infinite loops and allocation exhaustion terminate the evaluator
with bounded diagnostics. Host-only quantum expiry may retry a finite pure
prefix within a cumulative limit; it never rolls back commands already admitted.
Heap checkpoints are a possible later optimization, not an unproved first-stage
recovery guarantee. Continue-As-New bounds Temporal history, not PG replay cost.

## Context and data movement

| Context/state | What enters it and what survives recovery |
| --- | --- |
| Primary Thread | User objective, generated source/proposal, bounded progress and final Run result. Child transcripts do not automatically flow back per completion. Source generation still consumes model context/tokens. |
| JS evaluator | Admitted `args`, exact program and explicit result values. Ordinary variables are transient; frozen inputs plus journal replay reconstruct them. This is not an LLM context window and cannot be restored from a prose summary. |
| Invocation Thread | Pinned effective Agent/system/tools, exact script-computed task and selected upstream data, then that Thread's own provider transcript. No implicit parent/sibling transcript inheritance. |
| Durable result | Immutable schema-validated JSON/text or retained authorized references. Store before exposing to JS; never recalculate a committed value from a different model request. |
| Workspace/Memory | Explicitly accessed mutable files or shared knowledge. Sharing files does not share conversations, freeze those files or isolate Agents. |

Persist each admitted invocation's exact prompt, selected input/result
references and output schema with its effective Agent snapshot. Record any
normalization that affects model input; an input fingerprint alone is not a
context recipe. Existing provider transcripts remain the continuation authority
and the public event ledger remains an observation projection.

Reuse working-turn context admission and token estimation for planner, child
and synthesis requests. Existing primary/child compacted preparation snapshots
remain useful, but they do not yet audit every provider round. Complete
request recipes and equivalent Outcome/Advisor overflow recovery are follow-up
work; adding Workflow Runs does not silently establish them.

Conversation compaction may change a future request projection without deleting
the original transcript. It must not rewrite a program, a journal entry, a
committed invocation input or an Agent result. Any requested model summary of a
large intermediate result is a separate accounted Agent invocation, with its
own committed result; it is not an invisible replay-time convenience.

Validate and limit result size/depth, schema, individual prompts, aggregate
hydrated JS data, command count and final output. Reject over-limit values with
an explicit error rather than silently truncating a value used by JS logic.
Use bounded summaries and immutable references for large data. Scripts receive
references as data; Agents with authorized tools fetch required bytes. Protected
retention and ownership must outlive consumers. The first slice may use bounded
inline results without a new artifact service or automatic File publication.
Mutable workspace paths never substitute for replayable values.

The existing 1 MiB encoded content-Activity limit remains in force for Thread
execution. An ID-based program pump does not lift that limit or establish
unbounded model context/Temporal history; measure both the new IPC/journal
limits and existing Thread payloads for the supported journey.

Tool/provider content is untrusted even when passed by the program. Generated
prompts cannot expand the Run authority, become user approval, or carry ambient
credentials. Keep sensitive source/prompts/results behind Session authorization;
logs, spans and replay errors expose IDs, positions and reason codes by default.
Provide source/result inspection through authorized product surfaces; it must
not require asking a working model to reproduce the code from memory.

## Failure and recovery acceptance matrix

| Boundary | Required outcome and evidence |
| --- | --- |
| Model returned source, but admission never committed | No Run or child executes it. A bounded new planning request is a new accounted attempt; an unknown response is not recoverable by assertion. |
| Command committed; runner/Activity/Temporal acknowledgement lost | Same invocation and owned Thread/intent; retry returns the recorded acceptance. |
| Child completed while evaluator was absent | Receipt and wake persist; replay injects the same validated result once in the recorded delivery order. |
| Promise callbacks would run in a different order on replay | Detect or prevent it with the pinned scheduler/ordered journal; no new mismatching command may escape. |
| Script catches a rejected host call and starts fallback work | Record the rejection before exposing it; replay the same error/fallback trace even after policy/capacity changes. Authoritative host pauses and terminal controls cannot be caught to continue spawning. |
| Crash after a pure transformation or loop iteration | Recompute from fixed inputs/outcomes; next matching call retains its invocation identity. |
| Crash while committing a wait or Continue-As-New | A receipt cannot be stranded; reconcile PG version/inbox and pending wakeups. Continue-As-New retains only bounded coordinator references. |
| Evaluator lease changes while old Activity still runs | Stale-epoch writes rejected; a committed child is reused, not replaced. |
| Child has an ambiguous tool effect | Respect the existing operation journal and reconciliation rules. Do not invent a new Thread just because JS or an Activity restarted. |
| Definite child failure is caught by script | Committed typed failure can drive an explicit partial result or separately admitted safe retry. An automatic transport retry is not a business retry. |
| Program throws or returns while children remain open | Fence further effects, settle logical termination once, emit scoped cleanup; retain prior receipts and late actual usage. |
| Budget/approval/interrupt pauses the Run | Persist separate wait reasons. Replay cannot bypass them; lifting one does not clear the others or extend the absolute deadline. |
| Engine/profile unavailable after upgrade | Expose a capability wait with the pinned identity, subject to deadline; restore that capability or explicitly stop. Never replay old code under a silently changed profile. |
| Source is edited | Existing Run source remains immutable. A new admitted Run is explicit new work and may repeat effects; automatic suffix reuse/forking is deferred. |

Verify journal/admission rules with package and real PostgreSQL tests, and
coordinator crash/Activity-retry/Continue-As-New behavior with real Temporal
tests. Run scheduler-order and malicious/resource-exhaustion cases against the
selected real JS engine and isolation profile. Test the logical cancellation,
late-usage and actual worker cancellation boundaries separately. Never derive
all expectations from the implementation's own generated trace.

## Gate before a product implementation

The first follow-up is a bounded, independently reviewed feasibility study,
not a released executor. Select the engine/isolation/distribution combination
only after it proves: deterministic Promise turns and trace checking; hard
CPU/memory cancellation; blocked direct I/O/import/ambient entropy; restart at
every IPC/SQL boundary; stale-epoch rejection; and acceptable replay cost at the
chosen source/fanout/result limits on supported platforms.

Include rejection/catch/fallback replay under changed live policy, deferred
concurrency admission, unsupported orphan Promises and a rejected child whose
siblings continue. These are independent fixtures, not happy-path trace replay.

It must also settle the exact invocation result/error/schema contract, helper
membership/failure behavior, source wrapper, lease/quantum accounting and
runtime capability installation. If these checks fail, return to design
discussion. Do not replace them with a hidden JSON interpreter or advertise
resume based on a successful happy-path script. The remaining product PR groups
and lifecycle requirements are in the [main plan](workflow-runs.md#proposed-end-to-end-pr-groups).
