---
title: Dynamic workflow acceptance evidence
description: Actual JS pipeline evidence and the missing public completion loop.
slug: /design/workflow-dynamic-acceptance
---

# Dynamic workflow acceptance evidence

Mango does not yet provide a user-facing dynamic Workflow product, and this
experiment does not establish CMA parity. It runs an independently authored port
of the SDK cookbook's **computational pattern** through real QuickJS, PostgreSQL,
Temporal and Mango Threads with deterministic model responses. It does not run
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
| Run terminal result and primary notification | Missing: Run remains open and primary receives no report |
| Primary final answer and completion barrier | Missing; primary end_turn/Session idle can coexist with open Runs |
| Public Workflow tool, HTTP/events/SDK application | Unavailable; admission remains unexposed |
| CMA security review | Read only; its 20 → 3 → 3 review/controller application was not run |
| Production engine and strong isolation | Unselected/unqualified; one existing study profile is tested |
| Arbitrary programs and full lifecycle | Not established; operational, pause/stop/deadline/budget and retirement gates remain |

`TestQuickJSFactcheckLifecycleBoundaryRealServices` explicitly persists primary
`end_turn` with an open Run, adds a second open Run and pauses one. The paused
frontier rejects progress, but Session idle still coexists with open Runs and
zero Run-terminal events. This records a missing product boundary, not a working
completion guard. The computational tests likewise assert `state=open` and zero
primary report messages after candidate persistence.

The strict
[public CMA completion criterion](https://platform.claude.com/docs/en/managed-agents/workflow-runs#know-when-the-work-is-done)
requires all observed Runs ended plus a later non-interrupt primary `end_turn`,
rechecking if new Runs appear. Refusal may end an interaction but is not business
success. The security helper's additional refusal-as-idle branch is not adopted
as a success criterion.

The remaining user loop is candidate → fenced Run terminal result → durable
primary notification → later primary answer, including open/paused/second-Run
barriers. It requires lifecycle/event/API design and independent fault tests.
This port does not synthesize terminal events or enable a partial public feature
to claim that the complete cookbook application has run.
