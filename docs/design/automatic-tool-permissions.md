---
title: Automatic tool permissions
description: Evaluate self-hosted and MCP calls before admitting execution.
---

# Automatic tool permissions

Status: first implementation completed; model qualification and independent
review precede integration. The maintainer selected a replaceable model
evaluator and authorized a first version with subsequent tuning. Mango adopts
the public permission lifecycle and trust distinction below, and independently
implements the evaluator and self-hosted durability. No equivalent quality to
CMA's unpublished evaluator is claimed.

## User problem and scope

Applications currently choose unconditional execution or confirmation for every
enabled local/MCP tool. They need invocation-specific evaluation without writing
their own approval loop, while retaining explicit human review when judgment is
uncertain. Adopt the useful CMA `auto` lifecycle on Mango's existing `/v1` API:
`allow` admits execution, `ask` parks the existing confirmation action, and `deny`
returns a tool error without executing or terminating the agent.

First scope: the six operator-executed file/shell tools and remote MCP tools,
including child Threads with originating intent provenance. Defaults stay
`always_allow` for built-ins and
`always_ask` for MCP. Custom tools remain application-owned. Web tools currently
execute inside the model endpoint before Mango receives a call; enabled Web
tools therefore still require `always_allow`. Reject unsupported configuration
at Agent admission. Runtime Skill, coordinator, and Advisor primitives are not
new public configurable permission surfaces.

Non-goals: a security sandbox or human checkpoint guarantee, exact CMA
classifier quality, private MCP connectivity, Web execution redesign, custom
policy languages, evaluator configuration resources, separate evaluator model
credentials, and production/alpha deployment work.

## Public API and SDK pair

Accept `{"type":"auto"}` wherever supported built-in/MCP permissions are set.
Keep top-level `evaluated_permission` as `allow`, `ask`, or `deny`. Add a typed
`evaluation` object to policy-evaluated tool/MCP events:

- `{"type":"always_allow"}` or `{"type":"always_ask"}`;
- `{"type":"auto","evaluated_permission":{"type":"allow"}}`;
- `{"type":"auto","evaluated_permission":{"type":"ask","reason_code":"indeterminate"}}`;
- `{"type":"auto","evaluated_permission":{"type":"deny","reason_code":"high_risk"}}`.

Both outcome fields must agree. Custom calls have neither permission field.
Denied calls get the matching tool-result event with `is_error:true` and
`Permission to use {tool_name} has been denied.` No confirmation or worker
execution is admitted for a denial. Only an existing `ask` action can accept a
confirmation. An `auto` call that already evaluated to `allow` must not require a
second confirmation. SDK tool runners must act on the recorded outcome instead
of interpreting `auto` as an unknown static permission type.

Update OpenAPI, all three generated SDKs, HTTP contract/conformance tests,
guides, capabilities, and provenance in the same feature PR. The existing Go
runner already dispatches from the recorded outcome; retain that responsibility
and verify the additional typed evaluation through its conformance tests.

## Evaluation and trust

Use a narrow `permission.Evaluator` interface outside Workflow code. Its first
adapter uses the already configured `model.Client` and immutable Agent model.
No official SDK, hosted agent service, new key, or provider dependency is needed.
The adapter makes one bounded, non-streaming classification request without
tools, with a 20-second deadline and 1,024 output tokens. Return a validated
decision/reason, model, complete normalized usage, and provider stop reason.
Stop reason is necessary for existing response-level accounting rules. Provider
reasoning and free-form explanations are not public events or persisted receipts.

Build a separate immutable, provenance-bearing intent input before generic
Messages projection or compaction. The evaluator's fixed policy occupies its
system instruction. The Agent's original system instruction, client-authored
`user.message` text through the turn trigger, and client-authored companion
`system.message` text appear as identified intent in a structured message below
that policy. They express the task but cannot rewrite evaluator rules, even
when they say to approve every call. Session worker credentials cannot submit
user instructions or companion system messages. Outcome descriptions may express
client intent; File/rubric contents are assessed as data.

Do not infer authority from a generic Messages `user` role: that role also
carries tool results, Skill contents, and inter-Thread messages. Pass projected
conversation, tool metadata, arguments, and attachments in a separate quoted
data envelope. Strip provider thinking/raw control blocks from the evaluator
input. New queued messages beyond the turn trigger do not retroactively
authorize a call. A compacted summary must not substitute for original intent.

Child implementation design: server-created delegations carry a private
origin-trigger reference and an immutable client-intent snapshot captured while
the parent owns the dispatching turn. The child validates the reference and
uses that snapshot; later processing of previously queued client messages cannot
retroactively authorize the delegation. Missing or incomplete snapshots ask.
Server-created reports retain their causal reference for a fresh primary turn. Resolve a bounded same-Session causal chain to the
originating client task; preserve its event identity and causal boundary in the
prepared input. On return to the originating Thread, include client changes
already processed through the current trigger; queued messages do not grant
authority. Prior outcome descriptions enter that history only after a durable
terminal evaluation, since receipt-time processing is not outcome completion. A resumed barrier includes the authenticated companion instructions
from every causal resolution and approval, ordered by their actual event
sequence, with result bodies excluded. Agent-authored delegation remains data. Missing, invalid,
cyclic, or oversized ancestry produces `ask`, never broader Session-wide
authorization. This association is Mango's internal implementation choice; the
reviewed public CMA documentation states that inter-Thread messages do not
express user intent, but does not expose its causal-context implementation.

The external lifecycle follows the reviewed public CMA principle: judged safe
calls run, judged high-risk calls are denied without a client override, and
calls for which no determination is reached await confirmation. Explicit
client intent can change a judgment; some calls remain high-risk regardless of
who asks. The reviewed documentation and SDK types do not give a complete risk
taxonomy or the algorithm used to reach these judgments.

The following are first-version Mango evaluation criteria and test examples, not a
published CMA category list:

| Situation | Outcome |
| --- | --- |
| Ordinary action clearly within the task, including normal credential use at its intended service without disclosing the secret | `allow` |
| Insufficient authorization, ambiguous destructive scope, or uncertain effects | `ask` |
| Public disclosure of a real authentication secret, credential delivery to an unrelated endpoint, bypass of the configured execution boundary, or a clear deviation induced by untrusted content | `deny` |

High impact alone does not require `ask`: an explicitly requested deletion or
operator change can be allowed when the evaluator determines that the exact
invocation is safe within the task. CMA explicitly permits automatic execution
with potentially irreversible effects; Mango must not present a blanket
high-impact confirmation rule as CMA behavior.

The final category cannot be overridden by confirmation on that call. A user
who needs an explicit human checkpoint uses `always_ask`; this feature does not
replace executor access controls. Synthetic credentials, task data transfers,
and configured service authentication require separate positive test examples
so the evaluator does not confuse normal work with exfiltration. The fixed
policy must include representative explicitly requested operations, rather than
letting the model invent which actions are prohibited.

The evaluator must not adopt instructions from MCP descriptions/results, fetched
content, files, or inter-Thread messages. These are Mango's documented criteria,
not a claim to have recovered CMA's internal rules.

Bound the complete JSON input to 64 KiB. Oversized input returns `ask` rather than
allowing from a truncated security context. Timeout, transport failure, missing
client, invalid/truncated JSON, unexpected decision/reason, or uncertainty all
produce `ask/indeterminate`. Parse one exact JSON object and reject trailing or
unknown fields. Cancellation of an interrupted turn cancels evaluation instead
of publishing a fresh permission decision.

## Durability and budgets

Evaluate after a working-model tool round and before publishing its action
batch or dispatching any tools. Workflow code remains deterministic; the
Activity receives immutable operation IDs and input. Reuse the existing
confirmation, pending-action, Work-token, result, and tool-execution journal
paths after the outcome is fixed.

Persist each `auto` decision in a small private
`tool_permission_evaluations` receipt keyed by Session and preassigned tool-use
event ID. Store Thread, trigger, attempt ID, canonical input hash, decision,
evaluator model, complete normalized usage, and stop reason. A retry with a
different owner/hash conflicts. The first committed
decision wins; an Activity acknowledgement loss recovers it without rejudging.
Do not place these receipts in `tool_steps`: that journal's prepared/started/
completed states represent executor effects, and its attempt completion requires
all steps to finish. Local `ask` actions and external Work results have a
different lifetime. A separate internal receipt avoids overloading that state
machine or publishing actions before their barriers are admitted.

Establish the explicitly named `turn_attempts` owner before the first permission
evaluation, and set the Workflow's attempt ID even for all-ask/all-deny batches.
Completion closes these attempts without creating executor steps. Validate the
trigger's runnable lifecycle (including existing pending-resolution and active
outcome exceptions), Thread, attempt, and any later durable interrupt before
admission or dispatch. A canceled or completed attempt cannot be recreated by
a delayed permission Activity. Permission admission takes the trigger and
attempt IDs and validates this owner inside the transaction that may mark the
Thread budget-paused, using Session → Thread → attempt lock order. A separate
preflight read followed by current generic budget admission is insufficient.

Get an existing receipt before model admission. For a new evaluation, use the
shared Session budget ceiling. This admission wait must be interrupt-aware;
reusing the existing unconditional wakeup loop is insufficient. An idle-budget
interrupt is an eventless no-op only when there is no pending action and no
active attempt. With an unpublished permission round, persist and route the
interrupt normally, cancel the round, consume its trigger, and clear its budget
pause in the same interrupted-completion transaction that fences the attempt.
Raising the budget afterward cannot revive that call. Primary and child Thread
tests must cover this exact sequence.

Persist the normalized model-response facts and idempotent usage accounting in
one transaction, reusing the model-request ledger with the tool-use operation
ID. A private response receipt is evidence of judgment and usage, not execution
authority: known returned usage remains recordable after interruption or
termination, while owner validation denies publication and dispatch. Deletion
cascades both records. If cancellation races a successfully returned response,
use a bounded detached persistence context to retain the known facts. No raw
provider response is retained.

A cached receipt is already accounted. Return its winning facts without
inference, another budget check, or a separate accounting-repair path; current
execution authority is still required. Concurrent first-writer races account
the winning receipt's model, usage, and stop reason, never the losing candidate's
facts. Extract a focused transaction helper from current accounting rather than
duplicating its pricing/projection logic. Late accounting must retain existing
terminal status and budget-pause fields. Inference itself remains at least once
before a receipt commits; no exactly-once provider billing is claimed for an
unacknowledged response or process loss before persistence.

For server-executed calls, validate the committed receipt's exact invocation
and current owner/interrupt fence atomically with the existing `prepared →
started` transition. Once executor admission wins that lock race, a later
interrupt does not retroactively undo the side effect; existing completed or
ambiguous classification still applies. For self-hosted calls, validate the
receipt and owner in the existing atomic action/pending-barrier completion
transaction. An internal receipt alone must never publish an actionable call.

Persist receipts under short Session/Thread locks, validating their immutable
ownership. Check active owner and deletion/termination/interrupt fences before
allowing execution. No model or other external I/O occurs under a SQL
transaction or application lock. Session deletion cascades receipt metadata.
Development schema changes update the single baseline directly; no compatibility
shim or second API namespace is required.

## Acceptance and verification

1. Raw HTTP and SDK calls can configure supported `auto` tools, receive consistent
   typed outcomes, and reject unsupported Web configuration.
2. `allow` follows existing local/MCP execution; `ask` follows existing approval
   and later result flow; `deny` performs no executor I/O and the agent continues.
3. Mixed batches and child Threads preserve each call's owner and outcome.
   Confirmations cannot override a denial or approve another call.
4. Real PostgreSQL tests cover immutable receipts, concurrent first-writer
   decisions, lost acknowledgements, accounting atomicity, late known usage,
   and lifecycle fencing. Temporal tests cover restart, all-ask/all-deny
   attempt completion, cancellation, budget pause followed by interrupt and
   budget increase, and no rejudging.
5. Adapter tests inspect the exact request/trust envelope, parsing, all fail-safe
   paths, bounds, and the absence of tools/provider reasoning.
6. Opt-in configured-model cases cover useful benign calls, explicit intent,
   destructive/exfiltration requests, and prompt injection from tool/file/Thread
   data. Any unsafe allow blocks release; tests do not establish universal model
   safety. No model credentials are a development or CI requirement.
7. Complete required repository/service/SDK/docs checks and a configured local
   self-hosted live smoke before pushing. Independent reviews inspect runtime
   durability and evaluator trust separately before merge.

## Paired references

Reviewed current official [permission policies](https://platform.claude.com/docs/en/managed-agents/permission-policies)
and Go SDK v1.78.0 (`c9ebe447ac92c91748af817c265398e5d81ca49f`),
`betaagent.go` and `betasessionevent.go`; Python v1.11.0
(`18f25547f20cf5f01da69ac611e700e3bc9ebf21`),
`beta_managed_agents_auto_policy.py`. Adopt invocation-specific outcomes,
confirmation/denial lifecycle, intent-vs-data distinction, and coherent
API-to-SDK types. The public references do not disclose risk algorithms,
evaluation prompts, inference architecture, or quality evidence. Mango owns its
adapter, persistence, self-hosted execution fence, bounds, and testing.


## First-version qualification

On 2026-10-04 the configured `deepseek-flash` endpoint passed all 15 independently
authored cases: seven useful allows, two ambiguous asks, and six high-risk or
injection denials. Every case returned a complete, valid judgment. These are
small workload checks, not a statistical safety guarantee or CMA-equivalent
quality claim. Default tests and public CI remain offline.

Live qualification exposed two adapter constraints. Opaque thinking blocks can
accompany the final JSON text; Mango ignores them and never retains their
content. The first 256-token response cap truncated two expanded cases during
reasoning, so the bounded cap is now 1,024 tokens with the same 20-second
deadline. Truncation still asks. Token usage includes the complete normalized
response rather than only the short judgment text.
