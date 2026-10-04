---
title: Automatic permissions implementation plan
description: Implementation tasks and acceptance checks for invocation-specific tool permissions.
---

# Automatic Tool Permissions Implementation Plan

> **For agentic workers:** Use `superpowers:executing-plans` to implement this plan task by task. Steps use checkbox syntax for tracking.

**Goal:** Let Mango evaluate each supported local or MCP invocation, persist the result, and safely allow, ask, or deny it without inventing a second execution lifecycle.

**Architecture:** A replaceable evaluator calls the configured model outside Workflow code. PostgreSQL stores immutable response facts and usage atomically; current turn ownership and interrupts independently fence execution. Original client intent is prepared before transcript projection or compaction, including validated originating intent for child Threads.

**Tech Stack:** Go, PostgreSQL/pgx, Temporal, existing Messages model adapter, OpenAPI and first-party Go/Python/TypeScript SDK generators.

**Spec:** `docs/design/automatic-tool-permissions.md`. The maintainer authorized the first implementation on 2026-10-04, with subsequent tuning of evaluator quality. Correctness, cancellation, and fail-safe behavior are first-version requirements.

## Global Constraints

- Existing `/v1`; no compatibility shims or external SDK runtime dependencies.
- Support six local tools and remote MCP; provider-native Web tools require `always_allow`.
- One non-streaming evaluator request, 20-second deadline, 1,024 output tokens, no tools.
- Complete structured evaluator input at most 64 KiB; incomplete or oversized context asks without inference.
- Timeout, malformed response, refusal, unknown judgment, and transport error ask; parent cancellation cancels the turn.
- Agent/client intent is below the evaluator's fixed policy; tool, File, Skill, and Thread content cannot grant authority.
- Receipt plus model usage commit atomically; a receipt alone never authorizes execution.
- No external I/O under SQL locks. Session → Thread → attempt lock order for permission admission and executor admission.
- No hosted credentials in default tests or CI. Use configured local credentials only for explicit live qualification.

## Review Focus

- Compacted or bounded history must not promote summaries into authorization; incomplete original intent asks.
- Child reports and subsequent delegations must retain a validated causal task boundary, excluding later queued user input.
- A model response arriving after cancellation remains account-able but cannot publish an executable action.
- Interrupt during budget pause must consume the unpublished round; a later budget increase must not revive it.
- MCP aliases, mixed batches, and resumed confirmations must preserve exact invocation ownership and recorded outcomes.

## Task 1: Replaceable bounded evaluator

**Files:** Create `internal/domain/tool_permission.go`, `internal/permission/evaluator.go`, and `internal/permission/evaluator_test.go`.

**Interfaces:**
- Produces `domain.ToolPermissionDecision{Type, ReasonCode string}` and `domain.ToolPermissionEvaluation{Type string, EvaluatedPermission *ToolPermissionDecision}`.
- Produces `domain.PermissionIntent{AgentSystem string, Entries []PermissionIntentEntry, Complete bool}`; each entry identifies event, Thread, event type, and client text.
- Produces `permission.Input{Intent domain.PermissionIntent, Tool model.ToolSchema, Call domain.ContentBlock, Conversation []domain.Message}`.
- Produces `permission.Result{Decision domain.ToolPermissionDecision, Usage domain.TokenUsage, StopReason string, ResponseReceived bool}`.
- Produces `permission.Evaluator.Evaluate(context.Context, domain.Model, Input) (Result, error)` and `permission.NewModelEvaluator(model.Client) Evaluator`.
- Produces canonical input and invocation fingerprints for immutable receipt checks.

- [ ] Write `TestEvaluatorSafeResponseAndTrustEnvelope`: independently inspect the real adapter request, no tools, output bound, deadline, separate original intent/data, stripped thinking/raw blocks, and preserved normalized usage/stop reason.
- [ ] Write literal malformed-response cases: unknown/duplicate fields, trailing JSON, bad reason codes, max-token stop, refusal, tool response, transport failure, missing client. Assert `ask/indeterminate` rather than execution.
- [ ] Write `TestEvaluatorIncompleteOrOversizedContext` and `TestEvaluatorParentCancellation`: no inference for missing/oversized original intent; distinguish deadline fallback from parent cancellation.
- [ ] Run `go test ./internal/permission -count=1`; expected missing implementation failure.
- [ ] Implement the interface, structured trust envelope, fixed first-version policy, strict JSON parsing, bounded request and safe fallback.
- [ ] Run the package suite and race tests; expected PASS. Commit evaluator and tests.

## Task 2: Durable response facts, accounting, and owner fences

**Files:** Create `internal/pg/tool_permissions.go` and `internal/pg/tool_permissions_integration_test.go`; modify `internal/domain/tool_permission.go`, baseline migration, `internal/pg/budget.go`, `journal.go`, `store.go`, and `thread_completion.go` as needed.

**Interfaces:**
- Produces `domain.ToolPermissionReceipt` with Session, Thread, trigger, attempt, tool-use event, model-facing tool name, invocation/context hashes, decision, model, usage, and stop reason.
- Produces `Store.GetToolPermissionReceipt(ctx, candidate) (*domain.ToolPermissionReceipt, error)` and `Store.RecordToolPermissionReceipt(ctx, candidate) (domain.ToolPermissionReceipt, error)`.
- Produces `Store.EnsureToolPermissionAttempt(ctx, owner domain.ToolPermissionOwner) (TurnAttempt, error)` for a short, non-inference Activity before cancellation can interrupt the model call.
- Produces `Store.AdmitToolPermissionEvaluation(ctx, candidate domain.ToolPermissionReceipt) (bool, error)`; candidate includes the owner and exact cached operation identity, allowing atomic owner checking with budget bypass for already-accounted facts.
- Produces `Store.StartToolStepWithPermission(ctx, stepID, toolUseEventID string) error` for automatic server execution.
- Extracts an internal transaction-level accounting helper consumed by existing `AccountModelRequest` and receipt insertion, preserving current pricing and lifecycle projections.

- [ ] Write real-PG tests for first committed judgment, immutable owner/hash conflicts, receipt+usage atomicity, cache idempotency, concurrent candidates with different usage, and lost acknowledgment retry.
- [ ] Write real-PG tests for late response facts after interrupt/termination, refusal stop-reason pricing, deletion cascade, and no attempt recreation.
- [ ] Write real-PG tests for owner/interrupt validation atomically with budget pause and prepared→started; reject wrong invocation and non-allow receipt.
- [ ] Write budget-pause→interrupt→completion→budget-increase tests for primary and child Threads. Preserve idle no-op behavior when no action or active attempt exists.
- [ ] Run with `MANGO_TEST_DATABASE_URL` and watch the new contract fail; implement the smallest transaction helpers and private receipt table.
- [ ] Run `go test ./internal/pg -run 'ToolPermission|PermissionBudget' -count=1` with real PG, then the same tests under `-race`; expected PASS. Commit persistence and tests.

## Task 3: Original intent and child causality

**Files:** Create `internal/pg/permission_intent.go` and `permission_intent_integration_test.go`; modify `internal/domain/event.go`, `multiagent_runtime.go`, `thread_completion.go`, and evaluator input tests.

**Interfaces:** Produces `Store.PermissionIntentThrough(ctx, sessionID, triggerEventID string) (domain.PermissionIntent, error)` and a private server-authored origin-trigger reference for Thread messages.

- [ ] Write source-backed tests for original Agent/client text, companion system text, outcome description, exclusion of File/rubric/tool text as authority, and queued messages beyond the causal boundary.
- [ ] Write primary→child→report→follow-up tests; validate same-Session origin references and refuse missing, invalid, cyclic, or bounded-out ancestry with incomplete intent.
- [ ] Run tests and observe missing behavior; implement a bounded causal resolver using existing event/history primitives before generic projection. Keep origin references off HTTP/event DTOs.
- [ ] Run `go test ./internal/pg -run PermissionIntent -count=1` with real PG and relevant public event redaction tests; expected PASS. Commit causal intent and tests.

## Task 4: End-to-end auto orchestration

**Files:** Create `internal/temporal/tool_permissions.go`, `tool_permissions_workflow.go`, and corresponding tests; modify activities/types/worker/store source, workflow batch planning/resume, domain tool policy validation, and existing runtime tests.

**Interfaces:**
- Consumes Tasks 1–3. `PrepareTurnResult` records original permission intent when an auto tool is enabled.
- Produces a short owner-establishment Activity, followed by an interruptible permission Activity which checks cache, performs owner-aware budget admission, invokes the evaluator, and durably records response facts. Workflow records the established attempt before scheduling inference, including all-ask/all-deny turns.
- Produces deterministic Workflow evaluation before action publication or tool dispatch. Budget-paused outcomes wait for durable wakeups with interrupt checks; network evaluation uses existing heartbeat cancellation.
- Batch planning takes per-call committed judgments; server auto-allow passes receipt identity into atomic executor admission. Resume uses the recorded `ask` outcome, retaining ordinary confirmation and Work flows.

- [ ] Write domain admission tests for supported auto configurations and rejection of enabled provider-native Web auto.
- [ ] Write Workflow tests for local/MCP allow, ask, deny, mixed custom batches, all-ask/all-deny attempt closure, and no executor call on denial.
- [ ] Write Activity tests for cache recovery without rejudging, cancellation with known usage, and malformed/missing evaluator fallback.
- [ ] Write Workflow cancellation and budget-increase-after-interrupt tests; close private provider tool rounds honestly and never publish unadmitted Work.
- [ ] Run red tests; implement focused Activity/Workflow files and integrate batch outcome/resume checks without a new execution state machine.
- [ ] Run `go test ./internal/domain ./internal/agentruntime ./internal/temporal -count=1`, plus real-PG runtime service coverage; expected PASS. Commit orchestration and tests.

## Task 5: Paired public contract and documentation

**Files:** Modify `internal/httpapi/openapi.yaml`, raw HTTP tests, SDK generator outputs/conformance tests, `docs/api/events.md`, affected tool/Agent guides, `docs/capabilities.md`, and `docs/provenance.md`.

- [ ] Write independently authored raw HTTP and first-party SDK expectations for auto configuration, typed evaluation decoding, consistent top-level/nested outcomes, and confirmation rejection for denied/non-ask calls.
- [ ] Run tests and observe unsupported auto/missing schemas; update OpenAPI and regenerate first-party clients together.
- [ ] Preserve Go runner consumption of recorded outcomes; test auto allow/ask/deny decoding and dispatch without building additional helpers.
- [ ] Document a functional local/MCP auto example, unsupported Web auto, model-call cost/latency, fail-safe behavior, and first-version model-quality limits.
- [ ] Record official documentation plus actual reviewed Go/Python/TypeScript revisions; distinguish adopted lifecycle from Mango's own evaluator criteria and persistence.
- [ ] Run `make sdk-test sdk-conformance` and `make docs-check`; expected PASS. Commit contract and docs.

## Task 6: Qualification, review, and integration

**Files:** Add opt-in model evaluation fixtures/tests under `internal/permission`; update Makefile only if a dedicated explicit test command is needed.

- [ ] Independently author benign, explicit intent, high-risk, ambiguous, and tool/File/Thread injection cases; fixture secrets are synthetic. Positive cases must demonstrate useful allows, ambiguous cases ask, and unsafe allows block release.
- [ ] Run offline tests and explicit configured-model qualification. Record exact model and results without keys, payload secrets, or private provider reasoning.
- [ ] Run `make verify`, `make test-service-core` with temporary SeaweedFS endpoint, `make sdk-test sdk-conformance`, `make docs-check`, and `scripts/with-dev-env make test-self-hosted-live`; expected PASS or explicitly reported live limitation.
- [ ] Dispatch independent subagent reviews of whole-branch durability and evaluator trust. Fix material findings with red→green regression tests; keep adjacent roadmap items out of this slice.
- [ ] Create and attach a focused PR carrying rationale, acceptance criteria, provenance, verification, and model-quality limitations. Merge only after clean review and all required CI checks pass.
- [ ] Sync primary checkout while preserving the maintainer's existing documentation changes and image.
