# Repository instructions

Follow `CONTRIBUTING.md` for testing, durability, security, documentation, and
pull-request requirements.

## Product boundary

- Mango is an independent, self-hosted runtime for durable AI agents. Mango
  owns its public API, lifecycle semantics, roadmap, and release policy.
- Mango must never proxy or delegate Sessions, Files, sandbox execution,
  scheduling, persistence, or other runtime behavior to a hosted agent service.
- The current model adapter calls a Messages-shaped `/v1/messages` endpoint.
  That adapter is replaceable infrastructure, not a requirement that Mango's
  public API or future model integrations remain tied to Anthropic.
- External API documentation and SDKs may be used as clean-room design
  references. They do not define Mango's target contract and are not runtime
  dependencies. Development, CI, and production must not require hosted agent
  credentials.

## Development API policy

- Mango currently has no customers and no supported stable release. Until the
  maintainers explicitly change that status, backward compatibility does not
  exist as a product requirement.
- `/v1` is the single development API namespace. Change its routes, fields,
  schemas, and behavior in place when that improves Mango. Do not create `/v2`,
  dual behavior, deprecation windows, legacy shims, or translation layers for
  compatibility with an earlier commit or an external SDK.
- Existing tests, fixtures, database rows, and vendor-shaped fields are evidence
  of the current implementation, not compatibility obligations. Update or
  remove them with the design they cover. Development databases may be rebuilt;
  do not retain code solely to read data written by an earlier checkout.
- Keep an existing behavior only because it remains the right Mango design, not
  because changing it would be breaking. Only an explicit maintainer decision
  establishing a supported release or real customer migration can change this
  rule.

## CMA design research

- The latest stable public Claude Managed Agents documentation, API design, and
  public SDK changes are a standing research feed for Mango. Follow their design
  evolution deliberately: understand what problem a change solves and what was
  learned from it. This is a research obligation, not a compatibility
  obligation.
- The normal design workflow starts from Mango's current source, migrations,
  OpenAPI, documentation, and executable tests, then compares the analogous
  current CMA workflow. Differences are useful design evidence, not an
  automatic parity backlog and not a reason to preserve stale Mango behavior.
- Before finalizing substantial Mango work that has an analogous CMA workflow,
  review the current relevant CMA design alongside Mango's implementation and
  other useful references. Do not wait for CMA, and do not create work merely
  because CMA shipped something.
- Separate the design into four parts: the user problem, durable lifecycle and
  failure invariants, hosted/vendor constraints, and wire-level choices. Carry
  forward the first two when they help Mango; reconsider the latter two under
  Mango's self-hosted trust and operating model.
- Reuse and adapt useful surfaces deliberately. CMA routes, resource models,
  JSON fields, event types, and public SDK types may be design starting points
  when they reduce unnecessary invention and fit Mango's workflows. Mango does
  not need to rename or reshape a sound concept merely to appear different.
- When Mango and CMA solve the same user problem with the same lifecycle,
  prefer the same sound high-level design over gratuitous divergence. Exact
  field-for-field equality is not a goal: omit hosted rollout details and
  fields Mango does not need, and change a field when Mango's self-hosted
  semantics require it.
- Prefer standard HTTP semantics, widely understood data shapes, and existing
  Mango resource and event primitives. Introduce a Mango-specific header,
  wrapper, field, state, or abstraction only when a concrete requirement cannot
  be expressed clearly with an established convention or existing primitive.
- Once adopted, the resulting surface is owned by Mango. Similarity to CMA does
  not create compatibility, synchronization, migration, or release-timing
  obligations. Mango may change, remove, or extend that design directly on
  `/v1` when its users, operators, self-hosted trust boundary, or runtime need a
  different contract.
- Do not inherit vendor-only constraints by default. Hosted infrastructure
  assumptions, Anthropic headers and authentication, preview identifiers, SDK
  packaging, rollout timing, and endpoint inventory require an independent
  Mango rationale before adoption.
- For every material influence, record what Mango adopted, changed, or rejected
  and why in `docs/provenance.md` or the relevant design document. A CMA change
  can trigger design review, but it becomes implementation work only after a
  Mango user or operator rationale and acceptance criteria exist in the pull
  request, a design document, or an Issue.
- Validate the result through Mango's own HTTP, persistence, workflow, recovery,
  and service tests. Passing a third-party SDK test is optional research
  evidence, never the definition of success.

## Paired API and SDK design

- Treat CMA's public API and public SDK source as paired clean-room design
  references. The API expresses its wire contract and lifecycle; the SDK shows
  how that contract is mapped into language-idiomatic resources, methods,
  parameters, types, streams, and helpers. Study both sides for an analogous
  Mango workflow instead of reviewing either one in isolation.
- Preserve useful mapping relationships, not just isolated shapes. When Mango
  adopts or adapts an API concept, map it coherently into the Mango SDK; when an
  SDK pattern reveals an implicit lifecycle or error requirement, verify that
  Mango's API and runtime express it. Trace the concept from HTTP and OpenAPI
  through the generated client and its higher-level helpers.
- Where Mango and CMA retain the same resource and lifecycle semantics, prefer
  the same sound resource hierarchy, operation grouping, request/response and
  event types, pagination and streaming conventions, and division of helper
  responsibilities. A limitation or convenience of Mango's current generator
  is not a product reason for gratuitous SDK divergence.
- Mango's API and SDK remain one independently owned pair. The Mango SDK must
  faithfully encode, decode, and compose Mango's current public contract, and
  API or SDK changes should update OpenAPI, generators, documentation, and
  tests together as applicable. Exact CMA fields and syntax are not goals when
  Mango's self-hosted semantics require a different design.
- Use official SDK source for research only. Do not copy its implementation,
  execute it as a Mango client, add it as a dependency, call CMA, or require
  hosted credentials in development, CI, or production. Do not inherit hosted
  authentication, beta headers, rollout constraints, or translation layers
  without an independent Mango requirement.
- Validate the Mango API-to-SDK mapping with independently authored raw HTTP
  contract tests and Mango SDK conformance tests. Cover request encoding,
  response and error decoding, pagination, event streaming, and helper
  lifecycle semantics without deriving every expectation from the generator
  under test. Use comparison with the paired external references to find gaps,
  not to claim official-client interoperability or complete CMA compatibility.
- Record the reference versions, adopted mappings, intentional differences,
  and reasons in provenance or the relevant design document. Retain separate
  Mango persistence, workflow, recovery, and service tests for durability and
  side-effect invariants; successful SDK calls do not establish them. Keep
  cookbook applications separate from contract and system-test harnesses.

## Product-driven development

- Mango's documented HTTP API and observable runtime behavior define the
  product contract. GitHub Issues are optional coordination and roadmap tools,
  not a prerequisite for implementation. For a solo, short-lived change, the
  pull request may carry the problem statement, rationale, acceptance criteria,
  and non-goals directly.
- `docs/product.md` defines product direction. `docs/capabilities.md` records
  Mango's current capabilities and limitations; it is not a delta ledger
  against another service.
- Before substantial API, persistence, or runtime work, verify current behavior
  in source code, migrations, OpenAPI definitions, and executable HTTP,
  persistence, workflow, and service tests. Use Mango's implementation and
  documentation as the authority for current behavior.
- Select one user-visible, end-to-end problem. State its acceptance criteria
  and non-goals before implementation. A feature needs a Mango user or operator
  rationale; similarity to an external product is not sufficient.
- Implement the smallest safe slice that solves the selected problem. Expand
  internal architecture only when required for observable correctness,
  durability, recovery, security, or operability.
- Mango is pre-release. Public API, storage, and workflow changes may be
  breaking when they materially improve the product. Update code, migrations,
  OpenAPI, documentation, and tests together on the existing `/v1` surface.
- External contracts may be reused or adapted as starting points for resource
  models, routes, schemas, workflows, or edge cases. Record useful provenance,
  but make the resulting contract Mango's own, adapt it to the self-hosted trust
  boundary, and reject constraints that do not serve Mango users.
- Do not add research-preview or vendor-specific surfaces unless the change
  explicitly selects them for an independent Mango product reason.
- Stop when the acceptance criteria and required tests pass. Keep adjacent work
  out of the current slice; record it in a follow-up PR, design note, or Issue
  when coordination or longer-term tracking is useful.
- A completed user-visible change must update the affected API documentation,
  `internal/httpapi/openapi.yaml`, and the capability summary when applicable.
