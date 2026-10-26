---
title: Dynamic workflow acceptance evidence
description: Actual JS pipeline evidence and the missing public completion loop.
slug: /design/workflow-dynamic-acceptance
---

# Dynamic workflow acceptance evidence

Mango does not yet provide a user-facing dynamic Workflow product, and this
experiment does not establish CMA parity. It runs an independently authored port
of the SDK cookbook's pattern through real QuickJS, PostgreSQL, Temporal and
Mango Threads, including the primary synthesis turn, with deterministic model
responses. It does not run
the upstream SDK, a paid model, hosted CMA or the complete upstream application.

## References actually read

Source cells and saved outputs were inspected in
[`08_Dynamic_workflows.ipynb`](https://github.com/anthropics/claude-cookbooks/blob/a162e0184e71b60290808a43961a6cbc8060123c/claude_agent_sdk/08_Dynamic_workflows.ipynb)
and
[`CMA_agent_security_review.ipynb`](https://github.com/anthropics/claude-cookbooks/blob/a162e0184e71b60290808a43961a6cbc8060123c/managed_agents/CMA_agent_security_review.ipynb),
at pinned `a162e0184e71b60290808a43961a6cbc8060123c` and current checked commit
`bfe50e59dbb4f5546a0af9ca36ebb77e637c9ce1`. The security notebook is unchanged;
the SDK code changes only its default model. Its saved generated JS is identical,
SHA-256 `42672310bc61c2046375d1d877e391a38cb193e35590d025a1d26645a21d6b54`.
Both are under the repository's MIT license. Our fixture and assertions are
independently authored; no external SDK is executed or added as a dependency.

The SDK saved script exposes `agent`, `pipeline`, `phase`, `log` and module
metadata. It sorts extracted claims, pipes **each item** from verification into
a conditional skeptic, merges overturned evidence and waits for all item chains
before reporting. There is no global verifier barrier before skeptics. This is
a Claude Code/Agent SDK example, not evidence of CMA's private engine or journal.

The CMA security notebook uses public Session/Thread/custom-tool APIs. Its
application checks actual Thread system prompts, transcript ownership, all-page
read coverage and verdict structure. The saved run accepts 17/20, repairs three
missing reviews, then rechecks three inconsistent reviews. Intermediate idle is
not business success. These are visible application checks, not proof of CMA's
internal validator. That separate security application is studied, not executed
by this fact-check port.

## Reproduce

With Docker and **disposable** service endpoints:

```bash
MANGO_TEST_DATABASE_URL='postgres://test-user:test-password@127.0.0.1:5432/mango?sslmode=disable' \
MANGO_TEST_TEMPORAL_HOSTPORT='127.0.0.1:7233' \
make test-workflow-quickjs-integration
```

The target builds the existing pinned QuickJS study candidate and runs the
two-call proof, fact-check pipeline and lifecycle boundary probes. Fixtures own
their schemas, workflows and evaluator containers. No model credentials are
used. `MANGO_TEST_FACTCHECK_REPORT` optionally retains queried results, actual
report inputs, trace, usage and Run state after schema cleanup.

`TestQuickJSFactcheckPipelineRealServices` freezes actual JS in the Run request.
A pure local `pipeline` uses `Promise.all(items.map(...))` with per-item stage
overlap. The adapter executes this source; Go does not choose its commands.
Calls create real owned Threads through the existing frontier and outboxes.
Only committed receipt deliveries enter replay, one at a time.

Independent model checks and direct PG admission/receipt assertions require:

- Reverse-ordered extraction becomes ten sorted verifier admissions.
- Hold verifier 1; complete verifier 2; require skeptic 2's actual Thread to
  start while verifier 1's attempt remains active. A global phase barrier fails.
- Our fixture confirms IDs 2/3/9/10; skeptics uphold 2/3/9 and overturn 10. Only
  confirmed items get skeptics. The final row replaces the evidence, correct
  fact and reasoning, not just the verdict. This selected schedule is not a
  recovered upstream intermediate trace.
- No report admission while the last item chain is pending. Inspect exactly ten
  sorted, fully merged report-input rows and its actual terminal Thread receipt.
  The returned report includes claims, verdicts, evidence and correction advice.
- Exactly 16 business invocations: extract + ten verifiers + four skeptics +
  report. A zero-confirmed case has twelve invocations and no skeptics.
- Lose the ACK after a verification delivery commits, replace the actual Run
  SDK worker, replay the source and Continue-As-New. Owned Threads keep running
  on a distinct real Thread task queue. Retain business identity, result and
  durably known response usage; reject stale epochs. A second schedule holds a
  different final chain. Separately, an injected retryable error loses verifier
  3's returned provider response before its Activity result; the real Activity
  retries without another business admission. This does not simulate a Thread
  worker crash or a lost server ACK frame. Stable business identity and known
  response accounting do not imply exactly-once inference or complete knowledge
  of externally billed work after lost responses.

## Capability comparison

| Capability | What this experiment establishes |
| --- | --- |
| Real JS branching, fan-out, per-item pipeline, merge | Executed in isolated candidate against actual PG/Temporal/Thread coordination |
| Provider inference and source-file reads | Synthetic deterministic responses; no real-provider or source-reading quality claim |
| SDK schema option | Explicit JSON parsing in the port; no supported schema adapter or invocation contract |
| Pipeline helper | Local pure JS implementation, no public helper/ABI |
| Metadata, phase UI, log, model routing | Omitted, not silently emulated |
| Program return | Candidate persisted in the existing fenced journal |
| Run terminal result and primary notification | Implemented behind explicit internal finalization registration; one owned terminal delivery and primary Session wake |
| Primary final answer and completion barrier | Actual primary synthesis tested; open/paused Runs, pending deliveries and deferred Outcomes suppress premature final completion |
| Public Workflow tool, HTTP/events/SDK application | Unavailable; admission remains unexposed |
| CMA security review | Read only; its 20 → 3 → 3 review/controller application was not run |
| Production engine and strong isolation | Unselected/unqualified; one existing study profile is tested |
| Arbitrary programs and full lifecycle | Not established; operational, pause/stop/deadline/budget and retirement gates remain |

`TestQuickJSFactcheckLifecycleBoundaryRealServices` checks that a primary
`end_turn` is suppressed with an open Run, a second open Run or a paused Run.
The older computational-only registrations deliberately retain candidates so
those tests can isolate replay behavior. The separate internal `FinalizeResults`
registration enables the successful closure experiment described below; neither
registration is enabled by the default application or public admission.

The strict
[public CMA completion criterion](https://platform.claude.com/docs/en/managed-agents/workflow-runs#know-when-the-work-is-done)
requires all observed Runs ended plus a later non-interrupt primary `end_turn`,
rechecking if new Runs appear. Refusal may end an interaction but is not business
success. The security helper's additional refusal-as-idle branch is not adopted
as a success criterion.

## Successful Run-to-primary closure

`TestQuickJSFactcheckPrimaryClosureRealServices` now executes the same frozen
sixteen-invocation JS fact-check program through Run closure and an actual
primary model turn on the existing `SessionWorkflow`. It uses the independent
primary Thread projection and the real Thread runner. It tests lost finalizer
ACK plus Run-worker replacement, lost primary-consumption ACK plus primary
worker replacement, early replies, a second paused Run, refusal, active Outcome
resumption, revision and evaluation-budget exhaustion. Model behavior remains a
fixture, so program completion does not establish report accuracy or CMA parity.

| Transaction boundary | Durable fact and recovery rule |
| --- | --- |
| Program candidate | Existing fenced journal stores the bounded return value after every owned invocation has a delivered immutable receipt. A candidate alone does not finish a Run. |
| Successful Run finalization | Session then Run locks check physical coordinator, epoch, exact candidate cursor, deadline/control eligibility and settled receipts. One transaction closes the Run, stores its terminal value and unique delivery binding, appends the primary input, enqueues the primary Session outbox and retires obsolete Run wakeups. |
| Terminal ACK recovery | The exact persisted owner/epoch/cursor/pump identity recovers the terminal checkpoint before the ordinary closed-Run rejection path. A new/stale chain cannot restart it. Temporal receives only small terminal metadata and finishes its coordinator. |
| Primary consumption | The existing primary runner loads a PG-bound notification as data. Consumption commits with completion and requires the exact terminal response's message ID, nonempty text-only content and end_turn; earlier tool-round text, refusal, empty final responses, interruptions and requires_action do not count. A validated pending-action continuation retains the same delivery identity. |
| Aggregate completion | Under the Session lock, no open Run (including paused), unconsumed terminal delivery or waiting Outcome may be treated as finished. New Run admission, Outcome grading and conditional idle Work retirement use the same serialization boundary. Intermediate primary replies remain possible. |
| Outcome defer/resume | Before grading provider I/O, the typed Run barrier yields the existing primary loop. Working completion persists original Outcome/rubric, causal trigger, span identities and iteration. A bound result turn synthesizes with the retained context before grading. The current incorporated delivery is excluded only from its own grading barrier; other Runs/deliveries still block. |
| Outcome progress | Committed revision/end spans advance or finish the saved continuation with the same transaction. An ended span is not reused after a tool/approval park. Completion ACK retries distinguish a completed deferred working turn from a receipt-processed initial Outcome. |

The typed binding lives in `workflow_run_terminal_results`. The public envelope
uses the existing `system.message` content shape, with **no new HTTP/SDK field or
Run endpoint**. Trusted PG classification controls both turn selection and model
projection: Run values become clearly typed **user-role data**, not higher-trust
system instructions. A client-supplied marker cannot create that binding.
Ordinary system messages retain their existing behavior. Run-result text never
becomes authenticated user permission intent or widens tools/credentials.

The additive `20261026000003_workflow_terminal.sql` migration also stores durable
Outcome continuations. Only disposable test schemas are migrated in this study;
operators must use the normal explicit migration command before running a binary
against an older installation. Startup still performs no schema writes.

The refusal/interrupt/empty-final branches fail closed with an unconsumed result;
automatic redelivery/reconciliation after those controls is not implemented as a
product capability. Full stop/deadline/error terminalization, pause/resume control,
archive/repin protection, arbitrary-program qualification and production JS
isolation remain gates before public admission. The separate CMA security
review's twenty-session custom-tool and repair/recheck application remains
unexecuted here. A public application, real source-reading agents and paid model
acceptance are distinct from this internal deterministic closure proof.
