---
title: Workflow Runs proposal
description: Research and proposed delivery slices for durable parallel tasks and synthesis.
slug: /design/workflow-runs
---

# Workflow Runs proposal

**Status: proposal and research, 2026-10-18.** Workflow Runs are not implemented.
This document recommends a direction; it does not approve an execution boundary,
freeze a public contract, or change release acceptance. Implementation starts
only after the maintainer selects the boundary and the first slice's criteria.

## User problem and current foundation

An application needs to submit a batch of independent tasks to selected Agents,
run a bounded number concurrently, and receive a combined answer with references
to each result. It must recover after process replacement, explain partial
failure, and let an operator stop the batch without losing committed history or
charging the same usage twice. Manually watching many child Threads does not
provide that lifecycle.

The source baseline reviewed is `88ddec98b7e38b3e9f6b0a7e4f3fb9a5fdb45f99`,
including `internal/pg/migrations/20261005000001_schema.sql` and
`internal/httpapi/openapi.yaml`. The existing [multi-agent workflow](../guides/multi-agent.md) is useful foundation:

| Existing Mango capability and source | Reuse and missing boundary |
| --- | --- |
| Agent Version pins and Session-owned roster snapshots; `internal/app/agent_service.go`, `internal/controlplane/session_service.go` | Freeze selected definitions for a Run; later Agent edits must not change admitted work. A plan and its admitted inputs still need their own durable identity. |
| Independent Thread history, status filters, shared usage and lifecycle; `internal/pg/session_threads.go`, `internal/pg/multiagent_runtime.go` | Reuse execution and inspection. Add explicit Run ownership, logical task identity and committed result receipts; an arbitrary child idle or latest message is not a task result. |
| PostgreSQL ledger and at-least-once outbox; `internal/pg/outbox.go` | Commit scheduling decisions with their wakeups and deduplicate delivery. Existing cursor deduplication does not by itself deduplicate Run step creation. |
| Session and Thread orchestration; `internal/temporal/session_workflow.go`, `internal/temporal/session_thread_workflow.go` | Reuse replay, timers and Activity boundaries. Add Run scheduling, joins and cancellation recovery; a Temporal Workflow execution is infrastructure, not a public Workflow Run. |
| [Deployment Runs](../api/deployments.md), `internal/domain/deployment.go`, `deployment_runs` in the schema | These record a manual/scheduled attempt to create a Session, or its creation error. They are not a phase/step scheduler inside a Session. |
| Pending actions, interrupts, Outcome evaluation, budget and content guards | Extend the existing aggregate rules to open Runs; current code has no open-Run completion or Outcome barrier. |

Relevant existing tests include `internal/pg/multiagent_runtime_test.go`,
`multiagent_lifecycle_test.go`, `multiagent_interrupt_test.go`,
`internal/temporal/session_thread_workflow_test.go` and
`internal/httpapi/session_threads_sdk_test.go`. They establish their named Thread
invariants; they do not establish Workflow Run behavior. The schema and OpenAPI
have no public Run/phase/step model for this workflow.

## Paired external reference and intentional differences

The current [CMA workflow lifecycle](https://platform.claude.com/docs/en/managed-agents/workflow-runs)
describes background programs, phases and run-owned Threads. Runs remain open
through pauses; completion requires ended Runs followed by the primary answer's
end-turn, and Outcome evaluation waits for open Runs. Budgets, client waits and
absolute lifetimes interact. Reconnection reconstructs state from durable events;
there is no Run-list endpoint, and stopping is message-driven. A completed Run
is not proof that every business task succeeded. These are useful lifecycle
inputs, not a Mango contract or evidence about CMA's internal scheduler.

The [multi-agent configuration](https://platform.claude.com/docs/en/managed-agents/multiagent-orchestration)
enables workflows with predefined and/or inline Agents; the model decides when
to start one from a request. The paired public SDKs expose configuration, typed
`workflow_run.*` events, phases/results and a `workflow_run_id` Thread relation.
They do not expose a reusable program language, scheduler implementation or
local workflow execution helper. Public phase events do not define logical
steps or execution-attempt records. Copying SDK types would not deliver an
engine.

The latest stable SDK releases were checked against the official GitHub release
and tag APIs on 2026-10-18, and the following exact source objects were read
without executing the SDKs:

| Reference | Reviewed mapping |
| --- | --- |
| [Go v1.82.0](https://github.com/anthropics/anthropic-sdk-go/tree/543d266ce81a4487281e084b013fef807dbb5f18), `543d266ce81a4487281e084b013fef807dbb5f18` | `betaagent.go`, `betasessionevent.go`, `betasessionthread.go`: configuration unions, event/result variants and Thread association. |
| [Python v1.15.0](https://github.com/anthropics/anthropic-sdk-python/tree/bcd55fe1d3f0f1240765f8cef04295b67337f55a), `bcd55fe1d3f0f1240765f8cef04295b67337f55a` | `beta_managed_agents_multiagent_workflows_enabled_params.py` and Session Run/Thread models: predefined/inline settings and typed lifecycle events. |
| [TypeScript sdk-v0.135.0](https://github.com/anthropics/anthropic-sdk-typescript/tree/04a4c28c70de498d6d0e930e43edb5edb9ac545e), `04a4c28c70de498d6d0e930e43edb5edb9ac545e` | `beta/agents/agents.ts`, `beta/sessions/events.ts`, `beta/sessions/threads/threads.ts`: settings, discriminated events and Thread mapping. |

Mango can adopt useful lifecycle and resource relationships while choosing its
own limits and operator controls. Vendor defaults, hosted credentials, preview
identifiers, model restrictions and retry counts are not Mango requirements.
CMA's [self-hosted model](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
still uses its hosted agent/control plane. Mango must own scheduling, storage,
execution and recovery. A cloud sandbox is not required to schedule Mango's own
Agents on its existing self-hosted control plane and operator workers.

## Execution boundary to select before implementation

**Recommendation, pending maintainer choice:** admit a structured, bounded plan
and interpret only its validated operations in Mango's control plane. Initially
those operations are fixed-Agent fanout, join and synthesis. Model inference and
tools remain Activities or existing worker operations; Temporal code performs
trusted deterministic orchestration. Validation and execution are Mango-owned.

| Option | Benefit | Required boundary and tradeoff |
| --- | --- | --- |
| Bounded declarative plan | Durable transitions and limits can be inspected before execution; no new general code runtime. | Mango must define supported operations, typed references, bounded expansion, validation and replay semantics. It will express fewer programs than arbitrary code. |
| Program driven by an operator-owned Docker worker | Flexible libraries and arbitrary computation near operator data. | Needs isolated execution, a frozen interpreter/image/program, checkpoint and receipt protocol, lease fencing, replay behavior and external-effect recovery. Restarting a process is not resuming its program. This is a separate design and larger first slice. |

Never evaluate model-generated code in the API, orchestrator or Temporal worker
process. A Docker boundary alone does not make retries safe or establish
checkpointing; tools and generated programs may already have changed external
systems before a crash.

Also pending are the public start/inspect/stop surface, the relationship between
application-submitted and model-started Runs, and the approval gate for a proposed
plan. Deterministic application control is worth evaluating for self-hosted
operators; CMA's message-driven control is not a reason to omit it. Prefer
existing Session resources/events and standard HTTP, then select exact routes
and schemas with the first implementation. No speculative endpoint is approved
by this proposal. Inline Agent creation and arbitrary-code execution are outside
the recommended first slice.

## Proposed durable model and acceptance invariants

These are requirements to validate in the selected design, not implemented
fields or an approved schema:

| Concept | Durable responsibility |
| --- | --- |
| Run | Session/Workspace owner, frozen plan revision, Agent pins, admitted inputs, permission ceiling, limits, absolute deadline, usage and terminal result. |
| Phase | Stable progress grouping. A phase-end event alone must not imply successful task completion or a satisfied join. |
| Logical step | Stable task ID, dependencies and bounded input references. Expansion keys must survive duplicate wakeups and restart. |
| Execution attempt | A distinct attempt/generation, owning child Thread, execution state and settlement receipt. Transport retry must not silently become a new business attempt. |
| Result reference | A committed step/attempt receipt referring to owned Thread/events or authorized immutable Files, with bounded summary and explicit success/failure. References must retain their bytes and authorization while consumed. |

1. **Atomic ownership and delivery.** Commit each admission, spawn decision or
   result settlement with its ownership rows, public events and required outbox
   intent under consistent Session-first transaction locks. No model, worker or
   object-store I/O belongs inside those transactions. Crash between SQL commit
   and signal delivery must recover. Replayed scheduling must not spawn another
   child for the same committed logical attempt or settle a join twice.
2. **Complete the owned work.** A Run owns its child lifecycle and accepts each
   result only for the expected step and attempt. Joins consume settled receipts,
   including explicit failures. Terminalizing a Run must fence future spawn,
   settle or cancel its owned children and pending actions, and emit one terminal
   result without modifying unrelated ordinary Threads.
3. **Open-Run barrier.** No primary final completion or Outcome grading may
   treat an open Run as finished, including during budget or approval waits.
   Terminal results must be delivered durably to the primary synthesis turn.
   Only after required Run results are consumed and synthesis completes may the
   aggregate report final end-turn or start grading. Run completion and task
   quality remain separate facts. Work idle retirement must check this expanded
   aggregate too; an apparently quiet Thread is insufficient.
4. **Cancellation and late arrivals.** Define Run stop separately from targeted
   Thread interrupt and Session-wide interrupt; do not silently reinterpret
   current Mango interrupts. Cancel-versus-result, cancel-versus-spawn and
   timeout-versus-join need a transactional winner. Late receipts cannot reopen
   a terminal Run or charge usage twice; actual already-incurred usage remains
   accounted. Archive/delete and Agent repinning while Runs are open need
   explicit, atomic admission rules.
5. **Bounded waiting and spending.** Fix concurrency, total expanded steps,
   attempts, result size and an absolute deadline at admission. Approval waits
   remain open and cannot become implicit approval or success. Pauses must not
   silently extend the deadline; define how budget changes resume only eligible
   work. Concurrent model
   requests can overshoot a usage budget; specify reservation/admission policy
   and account actual usage without promising an exact cap after inference.
6. **Side effects and retries.** Infrastructure retries reuse committed identity.
   A failed child is not automatically safe to run again on a new Thread.
   Explicit business retry policy must distinguish definite failure from an
   unknown external effect, retain prior attempts, and require idempotent tool
   keys or reconciliation. Initial delivery may fail the Run instead of retrying
   an ambiguous task automatically.
7. **Payload and access limits.** Use small scheduling metadata and existing
   durable result references; do not accumulate full batch outputs in Temporal
   history or bypass the [runtime content limit](../api/events.md#runtime-content-capacity).
   Measure actual encoded Activity/completion payloads. Enforce Workspace and
   Session ownership, immutable pins, permission ceilings and worker leases on
   every expansion/result path. A plan cannot widen credentials or tool policy.
8. **Observable recovery.** Durable event history must reconstruct Runs, phase
   progress and owned children after disconnection. Use subscribe-before-history
   and ID deduplication. Events, errors and SDK variants must distinguish waiting,
   partial failure and terminal results without exposing prompts or credentials
   in diagnostics. Metadata-only tracing complements this ledger; it does not
   replace durable receipts.

Keep PostgreSQL as the source of lifecycle truth and Temporal as the durable
orchestrator. Reuse existing Thread execution rather than implement another
agent loop. New Run state must extend the current admission, pending-action,
Outcome and idle-retirement decisions coherently; adding a Thread field or a new
Temporal Workflow name alone does not satisfy these invariants.

## Proposed end-to-end PR groups

Each group is an independently reviewable user journey and acceptance gate,
subject to the boundary decision above. They may be split further if needed for
safe review; this is not a promise of complete CMA behavior in three PRs.

### 1. Fixed-Agent fanout, join and synthesis

Accept an explicit bounded batch using frozen predefined Agents. Persist the
Run, phases, logical tasks, attempts and owned children; schedule with bounded
parallelism; join their committed receipts and synthesize a combined answer.
Deliver usable start, inspection and deterministic stop behavior in the chosen
public surface, rather than exposing empty Run types ahead of the engine.

The first slice reuses the existing Session's shared sandbox and filesystem.
Concurrent model turns or Threads do not imply concurrent local tools, a separate
filesystem per Agent, or security isolation between Agents. Keep the current Go
worker behavior: `EnvironmentWorker` composes one `SessionToolRunner` for the
Session, whose dispatch loop executes local tool calls serially. Do not add tool
parallelism or per-Agent sandboxes as part of fanout. Either change needs a
separate design for file-write conflicts, external side effects, resource limits
and isolation boundaries.

Acceptance includes Workspace isolation, plan/pin freezing, successful parallel
results and synthesis, explicit failed-child reporting, approval/budget waits,
open-Run/Outcome barrier, stop/timeout and late-result fencing. Real PostgreSQL
and Temporal tests must replace processes or replay at spawn, receipt, join and
terminalization boundaries, deliver duplicate wakeups, and prove no duplicate
committed spawn or accounting. Cover Work lease reclaim while a Run stays open.
No branches, loops, inline Agents, model-generated plans or arbitrary programs.

### 2. Model-generated validated plans

Reuse the same admission and scheduler. Let a model propose only the supported
plan operations; validate and freeze the accepted plan before scheduling any
child. Preserve its proposal/admission outcome and explain rejected plans.
Decide explicit user approval versus preauthorized automatic admission; neither
is assumed by a configuration name or the model's own text.

Acceptance includes malformed/cyclic/oversized plans, unknown Agents or refs,
policy escalation, limit violations, approval denial and changed proposals.
Restart after inference but before admission must not silently regenerate a
different plan or spawn twice. Test planner failure and actual planner usage,
plus the same recovery/barrier path as group 1. No model-generated code, hidden
privilege expansion or new general execution runtime.

### 3. Bounded branches, loops and explicit failure policy

Extend the validated plan with a small selected predicate/iteration model,
strict expansion limits and persistent iteration identities. Record each
branch decision and join membership so replay cannot select a new path. Define
fail-fast versus explicitly collected partial failures, cancellation of siblings,
and allowed retry/backoff/attempt limits before enabling them.

Acceptance includes branch/loop restart, iteration exhaustion, failed or
ambiguous effects, partial joins, budget/approval pause, cancellation during
expansion and late prior-attempt receipts. Prove bounded total work and terminal
settlement under duplicate delivery. No unbounded recursion, arbitrary language
interpreter or claim of exactly-once external side effects.

Every group includes affected HTTP validation, OpenAPI, Go/Python/TypeScript
client mappings, independent raw HTTP and SDK conformance, API/capability docs,
and owning PostgreSQL/Temporal recovery tests in the same delivery. Add service
and operator-owned Docker coverage when worker execution changes, and record
an available real-model smoke separately. Schema/API work must not land as
claimed functionality before its runtime path and recovery acceptance exist.

## Separate self-hosted follow-ups

These candidates solve independent user/operator problems. Each needs its own
selected scope and acceptance; they are neither prerequisites for basic bounded
fanout nor an automatic parity backlog.

| Candidate | Existing capability and a bounded next acceptance |
| --- | --- |
| Opt-in metadata-only OTel | Current lifecycle logs and model/Outcome span events are not first-party Work/tool OTel tracing; indirect OTel dependencies do not supply that feature. Start with caller-supplied tracing for Go worker Work/tool lifetimes, including direct `HandleItem`, cancellation and lease loss. Keep tool arguments/results, prompts, credentials and arbitrary baggage out by default. Decide propagation/exporter separately. See the [official self-hosted tracing reference](https://platform.claude.com/docs/en/manage-claude/opentelemetry/sdk). |
| Recoverable File/Git preparation and controlled Files outputs | Operator staging and explicit immutable File upload already work. A helper could freeze checksums/commits and relative destinations, avoid overwriting local work on recovery, and publish only operator-allowed paths under scoped authority. Test interrupted/unknown uploads, size/hash checks, credential narrowing and lease loss; never put a Workspace key in a tool container. |
| Repository Skills import | Validated bundles and immutable Skill pins already exist. Import explicitly selected paths at a fixed repository commit with path/symlink/conflict checks, then reuse existing validation and pins. Avoid implicit repository-wide trust. |
| Provider-enforced Web policy | Mango supports provider-native Web with fixed permission requirements. Admit domain restrictions only when the selected provider actually enforces them; fail closed when unsupported and allow child policies to narrow only. Do not advertise prompt instructions as network enforcement. |
| OAuth permanent-refresh failure | Refresh, validation and rotation fencing already exist. Add a durable permanent-failure transition and one notification, separating transient errors and fencing stale credential generations. Test rotation races and redact secrets. |
| Operator-private MCP policy | Public-address SSRF rejection is the correct default. A separate operator policy could allow narrow hosts/ports or use a worker custom-tool bridge, with explicit DNS/redirect/network and credential boundaries. No blanket SSRF bypass or hosted tunnel dependency. |

The official [self-hosted resource guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes#session-resources)
currently rejects `file` and `github_repository` Session resources with `400`
and directs operators to host staging; its worker prepares Skills and Memory.
Cloud mounting/output behavior is not the baseline for self-hosted parity.
Recoverable staging and scoped publication are still worthwhile Mango developer
experience improvements when independently selected. Normal deliverables must
also be distinguished from Mango's existing generated MCP File publication.

Image/PDF model input, cloud sandbox providers and experimental external-state
cold restore are outside this proposal's implementation groups and follow-ups.
They do not block selecting a self-hosted Workflow Run design.
