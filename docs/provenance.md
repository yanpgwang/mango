---
title: Design provenance
description: Public design references, adopted concepts, and intentional differences.
slug: /provenance
---

# Design provenance

Mango records material external influences so its independent design remains
auditable. Public specifications and SDKs may be deliberately reused or adapted
for terminology, routes, resource shapes, JSON fields, event types, public
client types, workflows, and edge cases. Mango does not rename a sound concept
merely to appear different. Once adopted, the resulting surface is Mango-owned;
the source does not define its target contract or create compatibility,
synchronization, or release-timing obligations. Mango's documentation, OpenAPI
definition, implementation, and tests are authoritative for current behavior.

Mango's implementation, storage, scheduling, and runtime design are independent
and self-hosted. Public surface definitions may be design inputs, but external
implementation code and non-public types must not be copied, and an external
release is never an automatic roadmap.

Migration numbers in the historical entries below identify the implementation
at the time of that work. On 2026-09-30, the pre-release migration chain was
consolidated into the [current development schema baseline](deployment.md#development-database-baseline);
those historical upgrade and downgrade paths are no longer shipped.

## Explicit database migration role (2026-09-30)

- Reviewed Mango's PostgreSQL initialization, command entry points, local
  Compose startup, and schema tests alongside Goose v3.27.3's provider and
  PostgreSQL session-lock APIs and its
  [provider documentation](https://pressly.github.io/goose/documentation/provider/).
  Adopted provider-local configuration and a per-schema advisory lock for
  explicit, repeatable migration jobs; startup reads the ledger directly so
  checking an empty database never initializes it.
- Rechecked CMA's
  [self-hosted sandbox boundary](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes).
  CMA operates its orchestration and persistence; its public API/SDK exposes
  no equivalent Mango operator database workflow. Mango owns its database and
  process startup, so `mango migrate` is an independent operator decision.
  No CMA route, field, SDK helper, hosted credential, or rollout requirement
  was adopted for this slice.
- API, orchestration, Workspace, and API-key commands require the binary's
  applied migration versions without executing migrations. Local Compose runs
  the one-shot role first. Existing historical databases still require a
  rebuild; manual DDL validation and supported distribution upgrade/rollback
  procedures remain separate work. HTTP/OpenAPI and SDK contracts are unchanged.

## Coding agent workflow (2026-09-08)

- Reviewed the locally downloaded cookbook at
  `a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5`: `CMA_iterate_fix_failing_tests.ipynb`
  and the Docker self-hosted launcher, alongside official Go SDK v1.71.0
  (`de6914c544629b14a67c0695ce147edae6a291e0`) Session, Agent, Environment,
  Files and event-stream resource methods and the Environment worker helper.
- Adopted: create Agent/Environment/Session; upload inputs; observe tool calls
  through an open-before-send stream; continue a second turn; verify results;
  archive resources. Mango's Go client maps the same resource hierarchy to its
  own HTTP contract. The fixture and application are independently authored.
- Adapted: the application downloads Files into an operator-owned directory,
  binds it through `mango-worker docker --workspace-root`, and explicitly uploads
  the selected deliverable. A replacement worker uses the same Session directory.
  A fresh container runs pristine tests independently of the agent's claims.
- Rejected for this self-hosted tutorial: cloud provisioning, managed File
  mounts, automatic output publication, hosted credentials, beta headers and
  executing an official SDK as a Mango client. No public API or storage change
  is needed. Acceptance criteria are in the
  [demo design](design/coding-agent-demo.md); runnable instructions are in the
  [coding agent example](examples/coding-agent.md).

## Self-hosted boundary follow-up (2026-09-08)

- User/operator rationale and acceptance criteria are recorded in the
  [follow-up design](design/self-hosted-boundary-followup.md): preserve Memory
  teardown during normal shutdown, provide an explicit artifact upload/download
  workflow, and remove stale capability and test boundaries.
- Reviewed the current [CMA self-hosted lifecycle](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes),
  [Files workflow](https://platform.claude.com/docs/en/managed-agents/files), and
  [tool result semantics](https://platform.claude.com/docs/en/managed-agents/tools),
  alongside official Go v1.71.0 (`de6914c544629b14a67c0695ce147edae6a291e0`),
  Python v1.4.0 (`62de60b27d04f0927a0ccf0f2610597fafcfab6a`), and TypeScript
  sdk-v0.124.0 (`ba14b1f4fdf2e840a7b32297965342a099f6201d`) public source.
- Adopted: operator-owned file staging and output transfer, SDK-owned bounded
  Memory teardown, and separate queue/Session credentials. Mango keeps the Work
  lease renewing during cancellation teardown and gives its reference Docker
  container 120 seconds, with a longer Engine request deadline.
  That grace applies to ordinary shutdown of the current worker. After reclaim,
  the old attempt has lost its credential and is force-removed within a separate
  Engine request budget before its replacement starts. Waiting for the old
  attempt's Memory teardown would consume the new claim's starting lease.
- Changed: Mango has only immutable Workspace Files and no producer of hosted
  Session-scoped copies. Every ready File is downloadable by that Workspace's
  authenticated application. `downloadable`, `scope`, and `scope_id` are removed
  from HTTP, persistence, and SDKs. The multipart upload, binary download,
  bidirectional pagination, and caller-owned streaming body remain coherent.
  The unused File-read grant is also removed from Work authorization. No worker
  Files privilege or automatic output-publication mechanism is added.
- Rejected: preserving the hosted upload/output eligibility distinction without
  a Mango lifecycle that needs it; returning a sandbox path for control-plane
  MCP bytes that the external worker cannot access. Full-result MCP transfer is
  not implemented, and documentation now records truncation and binary limits.
- Removed the old official SDK test dependency. Independent raw HTTP golden and
  validation tests remain; first-party SDK resource and event mapping tests and
  PostgreSQL/S3/Thread lifecycle tests replace executable third-party research.
  No external SDK implementation is copied or executed as a Mango client.

## Self-hosted runtime boundary completion

- Reviewed the current public CMA [self-hosted guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes),
  [cloud sandbox reference](https://platform.claude.com/docs/en/managed-agents/cloud-sandboxes-reference),
  [Files](https://platform.claude.com/docs/en/managed-agents/files),
  [Memory](https://platform.claude.com/docs/en/managed-agents/memory), and
  [tools](https://platform.claude.com/docs/en/managed-agents/tools) alongside
  Mango's HTTP, OpenAPI, worker, persistence, and Temporal behavior.
- Mango adopts the common durable lifecycle: Environment Work, scoped Session
  credentials, lease fencing, event recovery, approvals, correlated results,
  immutable Skill pins, and Memory synchronization. Shell/file execution is an
  operator-worker responsibility; Web and remote MCP retain their distinct
  execution owners.
- Mango intentionally rejects CMA hosted-only behavior. It has no `cloud`
  Environment, control-plane provider registry, sandbox provider credentials,
  packages/network policy fields, automatic File/Git mounts, or automatic
  workspace-output publication. The public Session Resource union contains
  Memory Stores only, and the post-create File Resource routes were removed.
- Docker is the OSS reference launcher. CMA cookbook providers are evidence for
  a provider-neutral worker boundary, not a reason to compile their SDKs into
  Mango. Future provider examples must preserve the same Work protocol and keep
  provider configuration operator-owned.
- The removed pre-release adapter path included Docker, E2B, CubeSandbox,
  OpenSandbox, and Daytona. Their dependencies, provisioning tables, cleanup
  workflows, in-process tool runtime, configuration variables, and coding-agent
  exception were retired together. Mango's existing `/v1` contract changed in
  place, as permitted before a supported release.

## MongoDB query example

- User problem: let a self-hosted agent query an operator's database using
  ordinary container configuration. The example asks a replenishment question
  against synthetic inventory in a real MongoDB container.
- Reviewed CMA's public Docker self-hosted cookbook on 2026-09-10, alongside
  the local cookbook snapshot at `a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5`
  and Python SDK v1.4.0 at `62de60b27d04f0927a0ccf0f2610597fafcfab6a`.
  The Docker example supplies `MONGO_URI` through its launcher; the public SDK
  separates Work polling from per-item tool execution and lease management.
- Mango adopts that division using its own Go SDK: the example launches Docker
  and passes the business environment; `WorkPoller` owns Poll/Ack and
  `EnvironmentWorker.HandleItem` owns tools, heartbeats, and Stop. Mango's scoped
  Work payload travels through the example's stdin transport. The Workspace
  key remains in the host application.
- The example owns its input data, Docker image, and optional local database.
  It queries through Bash and `pymongo`, without an application-side database
  tool or a hosted runtime. It does not adopt Atlas search, the separate fraud
  review workflow, CMA credentials, or the official client's implementation.
- Acceptance: run the standalone application against a real model and a real
  MongoDB, inspect the tool call and replenishment answer, and clean up the
  resources it created. No API, SDK, core launcher, or runtime change is needed.
  No system-test or CI harness invokes this example; durability and recovery
  remain covered by Mango's independent runtime tests.

## Self-hosted default user path

- User/operator problem: an OSS runtime should lead users through the execution
  boundary it intends to support. Mango's API and quickstarts previously created
  a Mango-managed `cloud` Environment even though the product direction and
  worker implementation select operator-owned self-hosted execution.
- Reviewed the public [CMA self-hosted integration guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
  on 2026-09-07 alongside the cookbook and public Go, Python, and TypeScript SDK
  sources at `a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5`,
  `de6914c544629b14a67c0695ce147edae6a291e0`,
  `62de60b27d04f0927a0ccf0f2610597fafcfab6a`, and
  `ba14b1f4fdf2e840a7b32297965342a099f6201d`. CMA confirms the reusable
  Environment queue, trusted poller, per-Session worker, operator-owned
  File/Git staging, workspace outputs, and SDK-owned Skill/Memory lifecycle.
- Mango adopts that high-level lifecycle and maps it coherently across HTTP,
  OpenAPI, the three SDK quickstarts, HTTP quickstart, and examples
  that need no managed File/Git mounts. Unlike CMA's hosted-product default,
  omitting Mango's Environment config now resolves to `self_hosted`; this is an
  intentional OSS trust-boundary choice, not wire compatibility.
- The control plane continues to own model calls and durable orchestration. A
  text-only or application-custom-tool turn needs no Environment worker;
  shell/file calls wait for an operator worker. Mango does not auto-register or
  auto-provision a worker when an Environment is created.
- Non-goals for this slice were a new credential shape, provider launcher,
  automatic File/Git transfer, or workspace-output API. The subsequent runtime
  boundary completion above removed the legacy path and retired the coding
  tutorial that depended on it.

## Self-hosted Web execution and convergence scope

- Reviewed the public [CMA tool execution boundary](https://platform.claude.com/docs/en/managed-agents/tools#restrict-web-search-and-web-fetch-domains)
  and [self-hosted resource boundary](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes#start-a-session)
  on 2026-09-05 alongside the Python SDK worker and Session runner at
  `62de60b27d04f0927a0ccf0f2610597fafcfab6a` and the Docker cookbook at
  `a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5`. The SDK composes the six local
  shell/file tools with the Session event protocol; the documented Web tools
  execute server-side for either Environment type.
- Mango's concrete defect was declaring Web tools as external client calls
  while the first-party self-hosted worker implemented only shell/file tools.
  Mango adopts the same ownership split and reuses its configured Messages
  endpoint's existing native Web path. Web responses remain lossless in the
  private PostgreSQL transcript, including after an external-result wait and
  orchestration-worker restart. Provider-owned tools are absent from the local
  dispatch table, so a malformed ordinary Web `tool_use` cannot become a
  sandbox execution or external result barrier.
- Mango keeps the existing `always_allow` requirement and explicit dependence
  on the configured endpoint's native Web capabilities. It does not claim CMA's
  Web approval, domain-filter, or public Web-event behavior. The existing text
  and usage projections are unchanged. There is no CMA call, hosted credential,
  third-party SDK dependency, new provider service, or schema migration.
- CMA self-hosted Sessions reject File/Git resource mounts and leave file
  preparation and deliverable retrieval to the operator. Mango removed those
  conveniences from its mandatory CMA convergence prerequisites. They can be
  selected independently for a Mango workflow. The next convergence work is
  complete Docker/control-plane recovery evidence and a self-hosted default
  deployment, followed by removal of the transitional provider registry. A
  future operator-managed sandbox launcher can reuse the same Work protocol.

## HTTP transport

- Claude Managed Agents documentation and public SDK behavior informed early
  use of `x-api-key`, provider version and beta headers, and a provider-named
  worker correlation header.
- Mango uses standard `Authorization: Bearer` authentication and media types,
  retains the generic `request-id` response header, and exposes optional worker
  correlation as the `worker_id` query parameter. It does not expose provider
  rollout headers on its inbound API.
- The Messages adapter continues to send the headers its configured outbound
  endpoint requires. Public SDK source is research input only; independent raw
  HTTP, OpenAPI, and Mango SDK tests verify Mango's transport contract.
- Claude Managed Agents' agent-level `inference_geo` and the public
  [Claude data-residency design](https://platform.claude.com/docs/en/manage-claude/data-residency)
  prompted a focused review on 2026-08-27. Mango rejected request-time
  geography from its Agent and Session model configuration: it is a hosted
  provider routing policy, other model platforms express placement through
  different endpoints or deployment resources, and Mango's replaceable model
  boundary cannot enforce a portable meaning for it. Operators select and
  govern the configured model endpoint outside the Agent contract. The current
  Anthropic adapter reads a provider-reported response region only as an
  internal list-cost input; it never sends a geography request field.

## First-party SDKs

- The public [CMA quickstart](https://platform.claude.com/docs/en/managed-agents/quickstart)
  and [Session guide](https://platform.claude.com/docs/en/managed-agents/sessions)
  informed the useful client workflow: create resources, send user events,
  iterate live output, and retrieve durable results with language-native types.
- Mango adopts those developer-experience goals, but generates Go, Python and
  TypeScript bindings exclusively from its own checked-in OpenAPI document.
  Transport, pagination and SSE implementations are first-party code; no
  external SDK implementation or hosted code-generation service is required.
- The SDKs retain standard bearer authentication and explicit local endpoints.
  They reject vendor beta namespaces, hosted environment-key issuance and an
  SDK-owned Agent execution loop. Current server limits remain visible rather
  than being hidden behind compatibility shims.
- Automatic mutation retries are deliberately omitted: Mango does not promise
  a general idempotency-key contract. Raw event streams remain live-only, with
  history listing and event-ID deduplication required for reconnect recovery.
- Language transport tests and real-handler HTTP conformance validate SDK
  behavior. The latter uses test-only storage/model fakes and is not evidence
  of durable service execution or live-provider quality.

## Built-in Agent tools

- The public Agent Toolset shapes and executable cases in the pinned Anthropic
  Go SDK informed Mango's line-oriented `read.view_range` behavior: ranges are
  1-based and inclusive, and a non-positive end reads through EOF.
- Mango retains its existing `path`, `file_text`, `old_str`, and `new_str`
  fields where they remain clear. The public SDK is design evidence, not a
  field-for-field compatibility target or a runtime executor dependency.
- Mango advertises `bash.restart` and `bash.timeout_ms` only for `self_hosted`
  Sessions whose SDK toolset owns a persistent shell. The transitional
  Mango-managed executor still starts independent commands and keeps its
  narrower command-only schema; a shared tool name is not evidence that two
  execution owners support the same lifecycle.
- Mango caps each built-in `read` at 64 KiB inside the sandbox so untrusted
  files cannot make worker memory scale without bound. Larger files and
  persisted tool outputs use ordinary `bash` byte slicing (`dd`, `head`,
  `tail`, or `sed`), following the established coding-agent split between a
  line-oriented file viewer and a general shell rather than inventing a
  Mango-specific character-pagination field.

## Outbound Webhooks

- The public [Claude Managed Agents Webhook guide](https://platform.claude.com/docs/en/managed-agents/webhooks),
  current public API reference, and generated Go SDK types supplied the
  high-level event envelope, useful event names, thin-resource notification
  model, one-time `whsec_` secret, and delivery edge cases.
- Mango adopted the Standard Webhooks `webhook-id`, `webhook-timestamp`, and
  `webhook-signature` headers and HMAC input. It also adopted stable IDs across
  retries, a fresh attempt timestamp, any-`2xx` acknowledgement, three jittered
  attempts bounded to 5–120 seconds, transactional subscription scope, no
  backfill, no ordering guarantee, and immediate redirect/private-address
  disable semantics. The public
  [Standard Webhooks specification](https://www.standardwebhooks.com/) defines
  the signing convention; Mango's leased PostgreSQL dispatcher is independent
  implementation code.
- Mango changed endpoint management for its self-hosted boundary. `/v1/webhooks`
  provides Workspace-scoped CRUD and explicit secret rotation because Mango
  has no hosted Console. Secrets use the operator-mounted AES-GCM keyring,
  deliveries and exact signed bytes survive worker replacement, public egress
  is checked again at connect time, and terminal delivery state is retained
  internally for bounded cleanup.
- Mango retains `workspace_id` in notifications but omits the hosted
  `organization_id` because no equivalent Organization resource exists. It
  supports the Session and scheduled Deployment Run event subset backed by a
  current Mango lifecycle; it does not advertise broader CMA resource events
  merely because their names exist externally. Manual Runs emit no Run
  notifications, matching the useful scheduled-only distinction.
- Mango rejected Anthropic authentication and beta headers, Console-only
  management, hosted rollout constraints, SDK compatibility as a success
  criterion, and an invented duration for sustained-failure auto-disable. CMA
  publicly describes that trigger but not its threshold; Mango records the
  continuous-failure window and leaves a concrete operator policy as follow-up
  work. It also defers `deployment_run.started` until Mango has a real
  in-progress Run lifecycle rather than synthesizing an event around an
  immutable final record.

## File-backed Session messages

- The [Managed Agents event API](https://platform.claude.com/docs/en/api/beta/sessions/events)
  defines `user.message` document sources that reference a previously uploaded
  File by `file_id`.
- The [Files API guide](https://platform.claude.com/docs/en/build-with-claude/files)
  defines upload-once File resources, non-downloadable client uploads, and
  File references in message requests.
- The public `anthropic-sdk-go` source supplied request and response examples
  during early development. Its former executable test dependency was removed
  in the 2026-09-08 boundary follow-up; public source remains a research input.

Mango's bounded UTF-8 projection, private admission snapshot, S3-compatible
storage, and explicit rejection of multimodal File sources are local design
choices documented in [Files](api/files.md) and
[capabilities and limits](capabilities.md).

## File-backed Session Resources (retired)

This records the earlier design. The self-hosted boundary completion removed
these File Resources, provider dependencies, and automatic mounts. Applications
now stage inputs and transfer outputs explicitly; see [Files](api/files.md).

- The [Managed Agents Files guide](https://platform.claude.com/docs/en/managed-agents/files)
  defines independently copied File resources, their read-only presentation
  beneath `/mnt/session/uploads`, optional mount paths, and runtime add/delete.
- The public `anthropic-sdk-go` types supplied Session Resource request and
  response examples during early development. The former executable dependency
  and the tests using it have been removed.
- Remote File Resource behavior was implemented against pinned provider Go
  clients. The [OpenSandbox Go SDK](https://github.com/alibaba/OpenSandbox/blob/main/sdks/sandbox/go/README.md)
  and [Daytona filesystem guide](https://www.daytona.io/docs/file-system-operations/)
  define streaming upload/download, metadata and permission operations,
  directory management, and move/delete. The
  [CubeSandbox Go SDK](https://github.com/tencentcloud/CubeSandbox/tree/master/sdk/go)
  supplies the E2B/Cube-compatible whole-value file operations. These provider
  APIs were implementation dependencies rather than definitions of Mango's
  target contract.

Mango's former provider-owned marker format and retry algorithm were independent
local choices. The adapters stopped at writable sandbox-local copies; E2B and
Cube additionally buffered whole files. These implementation limitations were
retired with that execution path. The [current sandbox boundary](sandboxes.md)
uses operator-owned workers instead.

## Remote Session output export

- The pinned remote Go clients provide the filesystem directory, metadata,
  download, and delete operations used by their adapters. OpenSandbox and
  Daytona expose streaming readers; the E2B/Cube-compatible client currently
  returns whole values.
- Mango reuses those provider operations only as an implementation data plane;
  it does not expose provider file types or routes in the Mango API.

Mango's `/mnt/session/outputs` boundary, unique adapter-owned tar snapshot,
two-pass validation, close-time cleanup, S3 publication, and idle-event ordering
remain Mango-owned behavior. E2B and Cube adopt the same repeatability and
cleanup contract but buffer each archive in worker memory as an explicit
Preview limitation; their SDK similarity alone is not treated as evidence of
support, so they run the same credential-free and opt-in live conformance suites.

## Git repository Session Resources

- The public [Claude Managed Agents GitHub repository guide](https://platform.claude.com/docs/en/managed-agents/github)
  demonstrates the useful user-facing concepts of a repository URL, optional
  branch-or-commit checkout, a default workspace mount, and repository content
  available to a coding Agent.
- Mango adopted those generic concepts but owns a `git_repository` resource
  rather than a GitHub-specific resource. Mango added `resolved_commit` so an
  operator can audit the exact source frozen at admission.
- Mango changed the lifecycle for its self-hosted boundary: the control plane
  uses public-only egress to create a bounded immutable snapshot, stores it in
  Mango's S3-compatible object lifecycle, and restores it offline through one
  adapter-neutral pending/ready marker protocol. The sandbox worktree is an
  independent writable copy.
- CMA's public [scheduled Deployments guide](https://platform.claude.com/docs/en/managed-agents/scheduled-deployments)
  and Deployment resource union informed Mango's decision to reuse the same
  high-level repository template across direct Sessions and Deployments.
  Mango retains only the generic URL, optional checkout, and optional mount
  path. Each Run resolves a branch or default checkout afresh and then reuses
  the existing Session snapshot lifecycle; commit checkouts remain fixed. The
  Deployment itself therefore has no misleading `resolved_commit`.
- Mango retained CMA's `session_resource_not_found_error` only for a
  deterministically unavailable repository or checkout. Temporary DNS, TLS,
  transport, and upstream failures remain `unknown_error` Runs and do not
  auto-pause a schedule. This Run classification is deliberately separate from
  Mango's ordinary Session HTTP error envelope.
- Mango rejected raw authorization tokens, vendor authentication/header
  semantics, hosted clone caches, provider-side repository APIs, and automatic
  `.claude/skills` discovery. Private credentials require a future Mango secret
  reference. Submodules, LFS objects, runtime attach/detach, push/PR workflows,
  and repository Skill discovery remain separate product decisions with their
  own acceptance criteria.
- `github.com/go-git/go-git/v5` is a replaceable control-plane implementation
  dependency. It does not define Mango's HTTP contract, and no hosted agent
  credentials or services are required by development, CI, or production.

## Coding-agent scenario fixtures

This section records a retired pre-release experiment. The managed-sandbox
system test and standalone tutorial were removed when Mango adopted the
self-hosted-only boundary. The unused calculator fixture was removed during
the subsequent example/test boundary cleanup. This experiment must not be read
as a current capability.

- Anthropic's public
  [`CMA_iterate_fix_failing_tests` cookbook](https://github.com/anthropics/claude-cookbooks/blob/main/managed_agents/CMA_iterate_fix_failing_tests.ipynb)
  supplied the MIT-licensed `calc.py` and `test_calc.py` fixture and the useful
  do-observe-fix workflow. The retired system test and standalone example owned
  separate copies of the inputs, each retaining the source license. The example
  adapted the checks to standard-library `unittest` so it needed no sandbox
  package installation; the original assertions were retained.
- Mango adopted the user problem and acceptance outcome: expose immutable input
  files, let a coding Agent iterate in a writable sandbox, independently verify
  the fix, and publish the final source as a durable Session output.
- Mango changed the execution to its own PostgreSQL, Temporal, Docker, File
  Resource, tool-journal, event, and Session Output lifecycle. The service test
  uses a retry-safe deterministic model; a separate opt-in test runs the same
  outcome against the configured Messages endpoint.
- The live scenario enables only local coding tools (`bash`, `read`, `write`,
  `edit`, `glob`, and `grep`). It deliberately rejects Web Search/Fetch for this
  offline task at the Agent configuration boundary instead of relying on prompt
  instructions as a security control.
- Mango did not adopt CMA API calls, hosted sandbox behavior, exact event names,
  archive semantics, or field-level compatibility. The external notebook is a
  scenario reference, while Mango's observable outcome and executable tests
  define success.
- On 2026-08-31, Mango reviewed the current public notebook again while turning
  the then-current coding-agent example into a runnable
  Python SDK tutorial. The user problem is to run, inspect, and independently
  accept a complete coding task through the same public client used by an
  application, without translating HTTP snippets or depending on Go internals.
- Mango adopted stream-before-send observation, explicit input mounts, narrow
  tool configuration, downloaded-artifact acceptance, and resource cleanup.
  Mango changed recovery to merge persisted event history with a fresh stream
  by event ID; recovery never retries a mutation. A distinct output filename
  avoids confusing downloadable Session input copies with the repaired result.
- The application checks the download against the original local tests in a
  separate restricted Docker container, with no model/Mango credentials and no
  generated-code execution on the host. Session deletion, not archival,
  releases execution resources. Kept Sessions support read-only resume and
  require explicit cleanup. Docker is not a hostile multi-tenant security claim.
- The example accepts an observed failing check, an `end_turn` idle boundary,
  one bounded published artifact, and passing independent calculator checks.
  Its real-model Docker-backed journey passed on 2026-08-31 during the work
  recorded in [PR #188](https://github.com/yanpgwang/mango/pull/188). This is
  scenario evidence, not a general model-reliability or production-readiness claim.
- Mango deliberately separates cookbook applications from system tests. The
  example connects to a running deployment through the public SDK; it is not
  launched by Temporal tests or a dedicated example CI harness. System tests
  own their runtime/recovery assertions, setup, and fixtures. A few duplicated
  calculator inputs are preferable to coupling internal tests to a tutorial.
- The runtime loop remains server-owned. No hosted credentials, helper DSL,
  public SDK runner, API change, or storage migration was adopted.

## Human-in-the-loop custom-tool gate

- Anthropic's public
  [`CMA_gate_human_in_the_loop` cookbook](https://github.com/anthropics/claude-cookbooks/blob/main/managed_agents/CMA_gate_human_in_the_loop.ipynb)
  supplied the expense-approval user problem, the useful `decide` versus
  `escalate` split, and the custom-tool result round trip as design evidence.
- Mango adopted the application-owned action boundary: the model proposes a
  typed custom call, the Session becomes idle, and an application or human
  returns the correlated result before inference continues.
- The expense flow is a standalone Go SDK application. Its local decision
  journal simulates an external expense system and prompts a terminal user for
  review. Runtime persistence, crash, and concurrency invariants remain in
  separate deterministic runtime and service tests.
- Mango changed the hosted presentation behavior. One idle event exposes every
  action in the current barrier rather than a sliding window. Partial results
  are durably claimed without waking execution; the final result resumes the
  complete result round exactly once, including after worker replacement.
- The scenario uses Mango-owned synthetic inputs and copies no Cookbook
  fixture. Its executable contract is PostgreSQL atomic admission, Temporal
  recovery, duplicate-result rejection, and persisted Event ordering.
- Mango's Webhook slice can wake an application on `session.status_idled`;
  this small example polls paginated persisted history. Notifications and live
  streams do not replace the authoritative custom-tool barrier.

### SDK tutorial recovery review (2026-09-30)

- User problem and acceptance: stop the application at human review and resume
  the same Session; retain resource IDs and chosen decisions; reconcile every
  history page before resubmitting a saved result; make partial cleanup retryable.
  This gives a self-hosted operator an inspectable recovery path through ordinary
  Mango SDK resources without adding runtime APIs or business logic to core code.
- Paired references reviewed: the current official [event lifecycle and
  streaming guide](https://platform.claude.com/docs/en/managed-agents/events-and-streaming)
  and Go SDK `v1.76.0`, commit
  `ad865dfa3d1a8d2f4a7ad0d072011e811e9957a9`, specifically
  [`betasessionevent.go`](https://github.com/anthropics/anthropic-sdk-go/blob/ad865dfa3d1a8d2f4a7ad0d072011e811e9957a9/betasessionevent.go)
  and the Session tool-runner public surface. The lifecycle guide is a published
  Beta surface; it is research evidence, not a stable Mango compatibility target.
- Adopted mapping: `Sessions.Events.ListAutoPaging` for complete durable history,
  `requires_action.event_ids` for the barrier, and `Sessions.Events.Send` with
  `custom_tool_use_id` for correlated results. Existing Mango SDK types express
  these mappings directly. No external implementation was copied or executed.
- Changed helper responsibility: this tutorial owns a private local decision
  journal and explicit start/resume/cleanup commands. The journal records the
  simulated business decision before sending; a fresh history read reconciles
  ambiguous responses. This makes human approval and client restart visible,
  rather than hiding them in a generic runner. SDK and server internals are unchanged.
- Rejected hosted constraints: no beta headers, hosted credentials, external
  service calls, or provider-specific client setup. One operator and one Session
  are the bounds. Distributed coordination, automatic resource-create retries,
  external-business exactly-once effects, and a general workflow engine are
  non-goals. Unknown create outcomes retain their setup stage for manual inspection.
- Validation stays beside the application: real Mango SDK requests against an
  independently authored HTTP fixture cover pagination, partial completion,
  accepted/lost and unaccepted result responses, private atomic state, input
  cancellation, and cleanup retry. These do not replace SDK contract or runtime
  service tests, and no system harness imports or runs the tutorial.

## Multi-agent specialist team

- Anthropic's public
  [`CMA_coordinate_specialist_team` cookbook](https://github.com/anthropics/claude-cookbooks/blob/main/managed_agents/CMA_coordinate_specialist_team.ipynb)
  supplied the useful specialist-team user problem: a coordinator delegates
  role-scoped work, waits for reports, consults an Advisor, and synthesizes a
  final decision.
- Mango's real-model example adopts that high-level workflow but uses synthetic
  release-readiness facts and no Web, hosted data, or third-party integration.
  The client exercises Mango's public HTTP resources and inspects its persisted
  Event and Session Thread projections.
- Mango changes child completion semantics deliberately. Ordinary child Agents
  finish a turn and the runtime projects their report to the coordinator; they
  do not receive or need a hosted `send_to_parent` tool. Persistent follow-up is
  addressed through Mango's runtime-owned `send_to_agent` tool and the existing
  `session_thread_id`.
- The scenario verifies one real provider run with two ordinary children, a
  primary-only Advisor consultation, per-Thread usage, a final synthesis
  barrier, and an interactive follow-up. Deterministic service tests remain
  authoritative for retry, interruption, recovery, archive, and deletion
  invariants.
- Mango did not adopt the Cookbook's SDK calls, cloud Environment fields,
  bundled sales collateral, web-search dependency, hosted model restrictions,
  or exact response text.

## SDK-first documentation

- Reviewed the public [Claude Managed Agents Sessions guide](https://platform.claude.com/docs/en/managed-agents/sessions)
  on 2026-08-31. Mango adopts the useful documentation pattern of showing the
  same operation in selectable language examples alongside lifecycle prose.
- Mango uses its own Go, Python, and TypeScript SDKs plus HTTP examples. The
  runnable quickstart files are the single source of code snippets and are
  verified against Mango's HTTP handlers with test-only repositories and model
  behavior. A docs migration does not establish hosted-platform compatibility
  or expand runtime capability claims.
- Mango does not adopt Anthropic SDK packaging, CLI commands, beta headers,
  hosted authentication, supported-language inventory, or unsupported resource
  options simply because the reference page shows them.
- The site uses [Fumadocs](https://www.fumadocs.dev/docs), its Docs layout and
  neutral theme, with Mango's existing mark and orange accents. Static export
  and a bundled search index preserve the existing GitHub Pages operating model;
  no hosted documentation, search service, model credential, or Node server is
  required to serve the built artifact.

## Documentation reader journey (2026-09-07)

Reviewed Mango `main` at `8b7872a` before reorganizing the README and docs.
The acceptance criteria are a complete offline first Session, accurate
source-only resource SDK instructions, separate model and tool-worker setup,
and readable navigation without duplicate folder indexes. The change is
editorial; it does not change HTTP, persistence, scheduling, or recovery semantics.

- Adopted the native title/description/body, folder index, and Card patterns
  from the current [Fumadocs page conventions](https://www.fumadocs.dev/docs/page-conventions)
  and [Markdown guide](https://www.fumadocs.dev/docs/markdown), checked against
  the installed Core/Base UI `16.15.4` and MDX `15.4.0`. Cards use Mango's
  relative-file resolution and static Markdown exports; no hosted service or
  custom visual framework is added.
- Reviewed [Temporal's README](https://github.com/temporalio/temporal) and
  [Dify's README](https://github.com/langgenius/dify) as examples of short product
  introductions, actionable setup, and links to deeper documentation. Mango's
  README removes the architecture diagram and keeps implementation details in
  Architecture; wording and commands are independently authored for Mango.
- Reviewed the current [CMA overview](https://platform.claude.com/docs/en/managed-agents/overview)
  to clarify the managed-agent category: applications submit work while the
  runtime manages the agent loop, tools, and Session state. Mango's README and
  docs home both name the open-source, self-hosted alternative positioning;
  durability is supporting evidence for that execution model. Mango operates
  the full control plane as well as tool workers, retains its own API and SDKs,
  and does not claim an official implementation or drop-in compatibility.
- Reviewed the current public [CMA quickstart](https://platform.claude.com/docs/en/managed-agents/quickstart)
  and [self-hosted sandbox guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes),
  paired with the official Python SDK's
  [Environment resource source](https://github.com/anthropics/anthropic-sdk-python/blob/main/src/anthropic/resources/beta/environments/environments.py)
  on the review date. Retained the useful Agent/Environment/Session explanation
  and distinction between orchestration and external tool execution. Mango
  defaults to self-hosted execution, runs its own control plane, and documents
  its actual Workspace/Work credentials and Docker launcher. Hosted Console
  steps, Environment keys, vendor beta headers, and hosted SDK commands are not
  adopted. No external SDK implementation is copied or executed.
- Runtime claims come from Mango's Environment admission and HTTP tests,
  OpenAPI, current runnable SDK examples, Compose configuration, Docker
  launcher, and its composed integration tests. The docs distinguish the
  default self-hosted path from the transitional managed File/Git/output path.
  Static-site checks establish rendering and link correctness, not evidence
  that a model or production workflow was run.

## Typed custom Skill references (2026-09-30)

- Mango users need concrete Skill pins in Agent and Session responses and useful
  SDK types. Reviewed the current HTTP parser, stored snapshots, retention
  transactions, runtime admission, OpenAPI, and first-party SDK helper alongside
  the [CMA Skills guide](https://platform.claude.com/docs/en/managed-agents/skills)
  and its paired public SDK types: Go v1.76.0
  (`ad865dfa3d1a8d2f4a7ad0d072011e811e9957a9`), Python v1.9.0
  (`a7285e919ab79998d9380b3b57f6315b7860b8d8`), and TypeScript sdk-v0.129.0
  (`bf2058689f845dfb10e59bd9ebeb5cb4e9318a9d`). These were the current core
  releases checked on 2026-09-30; the TypeScript checkout also carries
  unrelated package tags.
- Retained the useful request-to-response mapping: omitted or `latest` request
  Versions resolve to a custom reference with `type`, `skill_id`, and a concrete
  `version`. OpenAPI and all three generated Mango SDKs now express that
  response directly. Removed the arbitrary JSON response union and persistence
  fallbacks for earlier development data; Mango has no supported stable release
  or customer migration obligation.
- Kept Mango's independently owned durability rules: active Agent and Session
  pins retain archives, unrelated Agent updates preserve pins, and every stored
  pin must name a ready custom Version. Exact expanded archive size is required
  for bounded admission; the former unknown-size sentinel is rejected in both
  application validation and the development schema baseline. Older development
  databases must be rebuilt; no upgrade or translation layer was added.
- Hosted managed catalogs, vendor authentication, beta headers, and external SDK
  implementations were not adopted. Validation uses independently authored Mango
  HTTP/SDK checks plus PostgreSQL retention, deletion, rollback, runtime, and
  recovery tests. Skills functionality and cookbook applications remain separate.

## Custom Skills

- The public [Claude Managed Agents Skills guide](https://platform.claude.com/docs/en/managed-agents/skills)
  describes version-pinned custom Skill directories, a required `SKILL.md`,
  supporting scripts and resources, filesystem paths announced to the Agent,
  and instruction loading when relevant.
- The public [Agent Skills overview](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/overview)
  describes progressive disclosure and treats executable Skill bundles as part
  of the Agent's trust boundary.
- Mango adopted those useful user-problem and lifecycle concepts: canonical zip
  validation, immutable Version pins, name/description discovery, on-demand
  `SKILL.md` injection, and supporting files in the sandbox. Mango owns its
  `/v1` resource contract, S3 archive lifecycle, Agent-scoped runtime paths,
  private dispatcher, recovery behavior, and provider capability admission.
- Materialization for E2B, CubeSandbox, OpenSandbox, and Daytona reuses the
  same Mango contract through their pinned official filesystem clients. Mango's
  worker-side validation, sibling staging publication, provider-owned marker,
  instruction checksum, write-tool denial, and shared conformance suite are
  local design choices.
- Mango did not adopt Anthropic beta headers, hosted authentication, the
  `anthropic` managed catalog, cloud-only repository scanning, rollout timing,
  or a requirement to mirror hosted/self-hosted feature differences. Repository
  Skills remain a separate product decision; Environment Worker activation is
  described below.

## Self-hosted custom Skill preparation

- Reviewed the current public [CMA self-hosted sandbox guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes),
  [security model](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-security),
  and the Go SDK `EnvironmentWorker` and `AgentToolContext` source at v1.69.0
  on 2026-09-04. The paired references establish the useful lifecycle: fetch
  one frozen Session snapshot with the item credential, keep the Work lease
  alive while preparing inputs, download pinned Skills before tool dispatch,
  and clean worker-owned directories after the item ends.
- Mango adopts that lifecycle for its independently owned Go worker and direct
  `/v1` contract. The per-Work token already authorizes only the Session and
  its immutable Skill pins. The worker accepts Mango's canonical zip archive,
  verifies the public Version's exact byte length and SHA-256 digest, bounds
  per-archive and whole-Session compressed bytes, expanded bytes, and member
  count, and rejects path escapes and non-regular members. It stages the entire
  tree as a fresh direct child of the canonical Workdir and replaces only the
  final `skills` entry, so an earlier tool-controlled symlink is never traversed.
- Mango deliberately extends preparation to resolved roster Agents. Primary
  and `self` copies share `<workdir>/skills`; external Agents use the stable
  opaque scope already recorded in model context. This avoids name/version
  collisions in a shared Session workspace. A permanent validation failure is
  fail-closed and durably terminates the Work, Session, Threads, and active
  attempts. Temporary HTTP, network, or object-store failures retry and then
  leave the lease for queue reclaim rather than silently Stop the activation.
  This distinction is stricter than CMA Go's tolerant logging because Mango
  must neither execute a partial snapshot nor lose an operator-recoverable turn.
- The Agent loop remains in Mango's control plane for both managed and
  self-hosted execution. Skill activation therefore reads and verifies the
  immutable canonical archive in object storage; it never calls back into the
  worker filesystem. The worker independently materializes the same pin so
  later `read` and `bash` calls can access supporting files. Self-hosted model
  paths are relative to the launcher's configured Workdir; cloud runtime paths
  remain concrete sandbox paths. This preserves one execution-environment
  abstraction without coupling Temporal to Docker or a future provider launcher.
- Mango adds its own `POST .../work/{work_id}/fail` lifecycle operation and
  `session_input_failed_error`; these are not claimed as CMA wire compatibility.
  They express Mango's PostgreSQL/Temporal invariant that a permanent external
  preparation failure must commit a terminal public history atomically, while
  a transient failure stays reclaimable. Mango also exposes archive size and
  checksum on Skill Versions because an untrusted transport must be checked
  against the frozen control-plane record.
- Mango does not adopt Anthropic credentials, beta headers, Environment-key
  fallback, tar archive variants, hosted infrastructure behavior, or vendor
  SDK code. Memory synchronization, File/Git preparation, health-check Work,
  and additional launchers remain separate slices. No persistence migration or
  public wire-shape change is required.
- Acceptance: unit and PostgreSQL tests cover primary, override, idle,
  Deployment, and roster admission plus immutable instruction loading and
  fail-closed extraction. The real Docker worker test covers scoped download,
  container-side supporting-file access, cleanup, lease renewal, cancellation,
  and re-preparation on a later activation.

## External tool approvals

- Reviewed CMA's [permission policies](https://platform.claude.com/docs/en/managed-agents/permission-policies)
  and the official [SessionToolRunner approval gate](https://github.com/anthropics/anthropic-sdk-python/blob/071efb619cfe195d74deb377e1dd14814643b2ca/src/anthropic/lib/tools/_beta_session_runner.py#L701)
  on 2026-08-31. Mango adopts the invariant that execution location must not
  override permission policy: external tools wait for approval before execution.
- The Mango user problem was an `always_ask` self-hosted call incorrectly emitted
  as `allow`. Acceptance requires a durable approval before result admission,
  no server-side external execution, denial without a client result, atomic
  duplicate rejection, complete mixed barriers, child routing, and recovery
  after worker replacement.
- Mango reuses `agent.tool_use`, `user.tool_confirmation`, `user.tool_result`,
  and the original public tool-use ID. An allow advances the existing pending
  record to await its result; it does not resume the model. A persisted approval
  receipt makes this independent of SDK memory and stream availability. Denial
  follows the existing confirmed-tool error-result path without execution.
- The self-hosted trust boundary remains Workspace-scoped trusted workers.
  This slice does not introduce hosted credentials, a tool runner, resource
  preparation, automatic side-effect retries, or exactly-once execution claims.
- Migration 38 adds the internal approval receipt. It does not rewrite old
  incorrectly allowed events into approvals. Rebuild development databases
  rather than rolling back across unresolved two-phase calls; dropping approval
  evidence cannot preserve the new lifecycle. No compatibility shim is added.

## Provider-neutral self-hosted worker foundation

- User/operator problem: provider names had become mixed into Mango's core
  execution path even though a self-hosted Environment needs one stable Work
  protocol and provider-specific launchers outside the control plane. Without a
  first-party polling helper, every Docker or remote-compute example would
  independently reimplement claim, Ack, drain, reclaim, and Stop semantics.
- Reviewed the public CMA
  [self-hosted sandbox guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes),
  the paired Go SDK `WorkPoller`, and the
  [self-hosted sandbox cookbook](https://github.com/anthropics/claude-cookbooks/tree/main/managed_agents/self_hosted_sandboxes)
  at commit `26b5cdce81d357596f5df7f44f50908a80be40cf`, and
  `anthropic-sdk-go` v1.69.0 at commit
  `6298207eac7ff589e7fcc8a78f6c034ab09de47f` on 2026-09-03. The useful design
  is the separation between a provider-neutral worker protocol and thin Docker,
  Cloudflare, Modal, Daytona, or Vercel launchers. The compute providers expose
  generic infrastructure; they do not define the managed-agent lifecycle.
- Mango adopts that separation and the pull-style Go iterator relationship:
  Poll is tentative, Ack precedes yield, and drain ends normally on an empty
  queue. Mango does not adopt the reference poller's default auto-stop yet.
  At that slice Mango's Stop transition was not owner-fenced and could discard a
  queued or starting activation after an ambiguous Ack. The Mango poller
  therefore never stops Work; the next credential slice below makes a reclaimed
  item token unusable. Mango uses its existing routes, generated types, and
  error conventions rather than hosted beta headers.
- Acceptance for this first slice: the standalone Go SDK exposes a
  provider-neutral Work poller with option validation, long-running and drain
  behavior, cancellation, reclaim and worker query parameters, Ack-before-yield,
  strict empty-queue decoding, identity validation, no Stop after ambiguous Ack,
  and jittered retry. Unit tests use an HTTP server and do not execute examples
  or contact a sandbox provider; a PostgreSQL test proves an acknowledged item
  is reclaimed after its starting lease expires.
- Intentional limits at delivery: this was not `EnvironmentWorker` or
  `SessionToolRunner`; it did not heartbeat, execute tools, prepare resources,
  or claim exactly-once side effects. Environment-scoped credentials and Work
  secrets were deferred to the following slice.
  The old Mango-managed provider path remains until an independently tested
  Docker worker replaces its OSS workflow; no API or persistence change occurs
  in this slice.

## Per-Work Session credentials

- User/operator problem: an untrusted Session sandbox must execute its own tool
  loop without receiving a Workspace-wide key, and a worker whose lease was
  reclaimed must lose the ability to heartbeat or stop the new activation.
  `worker_id` remains operational metadata rather than an authorization proof.
- Reviewed Claude Managed Agents [Work documentation](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
  and the [Go SDK v1.69.0 worker source](https://github.com/anthropics/anthropic-sdk-go/blob/6298207eac7ff589e7fcc8a78f6c034ab09de47f/lib/environments/worker.go)
  at commit `6298207eac7ff589e7fcc8a78f6c034ab09de47f` on 2026-09-03.
  The paired reference clarified a detail not obvious from the resource schema:
  Poll's `secret` is URL-safe base64 JSON carrying a `sessions_token`; Poll/Ack
  use the Environment credential, while per-item heartbeat, Stop, Session
  execution, and input download calls switch to that bearer token. The payload
  is populated only by Poll.
- Mango adopts that credential handoff and wire relationship. Poll creates a
  fresh 256-bit `sessions_token`, returns it inside the same base64url payload,
  and stores only its SHA-256 digest. Ack has no body and every non-Poll Work
  response redacts `secret`. The token is accepted only for the exact Work's
  Heartbeat/Fail/Stop, the exact Session's read/event routes, and File/Skill inputs
  relationally pinned to that Session. A Workspace API key retains operator
  authority; Mango has not yet introduced a distinct Environment polling key.
- Reclaim rotates the token, so the former bearer fails authentication before
  reaching a lease mutation. The current owner's expired heartbeat returns
  `412`; the timestamp compare remains the optimistic-concurrency guard. Ack is
  idempotent in `starting`, allowing a safe retry after a lost response, and the
  Go WorkPoller retains the Poll payload across Ack's redacted response.
- Mango independently caps a requested lease TTL at 300 seconds. A healthy
  worker renews continuously, while an unbounded TTL would expand stale-owner
  access and can overflow database interval arithmetic. The token is usable
  only after Ack; active and starting leases expire from their last ownership
  timestamp, and a graceful Stop retains access only through
  `stop_requested_at + ttl`. Established Session SSE connections revalidate the
  database lease once per second and close after expiry, Stop, or token rotation.
- The execution credential may send only `user.tool_result` and
  `user.custom_tool_result`. Mango rejects companion `system.message` from this
  trust boundary because a sandbox tool runner has no reason to persist
  higher-priority instructions.
- Migration 39 adds the nullable, unique indexed token-digest column. Existing
  unclaimed development Work rows receive a token on their next Poll and any
  pre-existing TTL above the new bound is clamped to 300 seconds. Rolling the
  migration back removes the scoped credential and TTL constraint and is not a
  safe mixed-version deployment; Mango has no supported release requiring a
  shim.
- Mango rejects hosted beta headers, organization credentials, fallback to a
  broad key inside the Session sandbox, and undocumented access to other
  Workspace resources. The Session runner, composed Environment worker, and
  scoped Environment polling credential were left as separate slices.

## Provider-neutral Session tool runner

- User/operator problem: each self-hosted launcher otherwise has to recreate a
  lossless-enough Session event loop, permission gate, tool/result mapping, and
  lease-loss behavior. Divergent provider examples would make recovery and
  security depend on the selected sandbox rather than on one SDK primitive.
- Reviewed the public Claude Managed Agents
  [Sessions documentation](https://platform.claude.com/docs/en/managed-agents/sessions),
  [Go SDK SessionToolRunner source](https://github.com/anthropics/anthropic-sdk-go/blob/6298207eac7ff589e7fcc8a78f6c034ab09de47f/betasessiontoolrunner.go),
  and [Go SDK tool documentation](https://github.com/anthropics/anthropic-sdk-go/blob/6298207eac7ff589e7fcc8a78f6c034ab09de47f/tools.md)
  from v1.69.0 at commit `6298207eac7ff589e7fcc8a78f6c034ab09de47f`
  on 2026-09-03. The useful paired design is stream-first attachment followed
  by durable history reconciliation, serial local dispatch, explicit mapping
  of normal/custom tool uses to their matching result events, durable approval
  gates, bounded retries, and a pull-style iterator for observability.
- Mango adopts those lifecycle relationships in an independently authored,
  standard-library-only Go helper. It uses Mango's generated event unions and
  flat `Client`, reconnects with jitter, ignores server-side MCP calls, copies
  `session_thread_id`, and checks history after an ambiguous result write before
  retrying. A terminal/deleted Session and end-turn idle have distinct sentinel
  errors. Unregistered calls remain pending for split ownership rather than
  being incorrectly answered.
- Mango changes the execution boundary for self-hosting. `SessionToolCall`
  exposes the durable `tool_use_id` as an explicit idempotency key, and scoped
  request failures (`401`, `403`, or `412`) become `ErrSessionLeaseLost` without
  credential fallback. The local `SessionTool` interface requires only name and
  execution: schema and description already belong to the immutable Agent
  snapshot, so requiring a Messages-API tool definition again would create two
  authorities. Tools must honor cancellation; event reconciliation cannot make
  a non-transactional external side effect exactly once. The runner deliberately
  does not heartbeat Work, Stop Work, decode Work secrets, create a sandbox, or
  close caller-owned tools. Those belong to the later composed Environment
  worker or provider launcher.
- Acceptance: HTTP-backed SDK tests prove stream-before-history recovery and
  overlap deduplication, approval/denial/unknown-policy behavior, normal and
  custom result mapping, MCP and unowned-tool separation, ambiguous committed
  result recovery, cooperative cancellation on lease loss with no later write,
  end-turn idle, option validation, and race-free operation. No example is
  imported by the test suite, and no hosted credential or sandbox provider is
  required.

## Provider-neutral Environment worker composition

- User/operator problem: a Work poller and a Session tool runner still leave
  every launcher to implement the most safety-sensitive seam itself: scoped
  credential handoff, first-heartbeat admission, continuous lease renewal,
  cancellation after lost ownership, and final Work Stop. Divergent copies
  would allow a provider choice to change Mango's recovery and security
  semantics.
- Reviewed Claude Managed Agents' public self-hosted worker documentation and
  the `anthropic-sdk-go` v1.69.0 `EnvironmentWorker` at commit
  `6298207eac7ff589e7fcc8a78f6c034ab09de47f` on 2026-09-03. Mango adopts the
  useful composition boundary: Poll/Ack remain supervisor operations; the
  acknowledged item's token authorizes heartbeat, Session execution, and Stop;
  heartbeat and the Session runner run concurrently; ordinary exit force-Stops
  the item; known or presumed lease loss cancels execution and skips Stop.
- Mango changes the security default and startup ordering for its self-hosted
  trust model. A missing or malformed `sessions_token` is fatal rather than a
  reason to fall back to a Workspace credential. The first conditional
  heartbeat must succeed before any tool runs. Its returned TTL bounds the
  Session runner's ambiguous-result recovery, and repeated transient heartbeat
  failures are bounded by the last known lease TTL rather than retried forever.
  Heartbeat intervals account for Mango's one-second minimum TTL instead of
  waiting until that shortest lease is already expiring.
- `EnvironmentWorker.Run` provides the trusted, single-process composition.
  `HandleItem` provides the narrow sandbox-side entry point for a launcher that
  Polls and Acks elsewhere; it accepts only Work, Environment, and Session IDs
  plus the opaque Work secret. Non-secret IDs may come from environment, but
  the Work secret is now always explicit because environment is not a safe
  launcher transport when untrusted code shares the runner's process identity.
  Neither path creates compute, prepares File/Git/Memory inputs, closes
  caller-owned tools, or introduces a provider SDK. Custom Skill preparation is
  now the shared worker behavior described above. Environment-scoped Poll
  credentials are implemented as documented below; supervisors still own
  Docker daemon access and remain trusted infrastructure.
- Acceptance: HTTP-backed tests independently verify supervisor-versus-item
  bearer separation, first heartbeat and forced Stop, serial Session tool
  execution, cancellation with no result or Stop after `412` lease loss,
  bounded failure when no heartbeat ever succeeds, strict secret/configuration
  validation, and race-free operation. No hosted credential, provider, example,
  API change, or persistence migration is involved.

## First-party Docker self-hosted worker

- User/operator problem: Mango had the Environment Work protocol and
  provider-neutral SDK lifecycle, but no runnable OSS launcher proving that a
  real sandbox can preserve those semantics. The existing Docker provider runs
  inside Mango's legacy managed worker and therefore cannot validate the target
  trust boundary by itself.
- Reviewed the public Claude
  [self-hosted cookbook](https://github.com/anthropics/claude-cookbooks/tree/a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5/managed_agents/self_hosted_sandboxes),
  [self-hosted security guidance](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-security),
  `anthropic-sdk-go` v1.69.0 at commit
  `6298207eac7ff589e7fcc8a78f6c034ab09de47f`, and current SDK `main` at
  `e9c104e7e5fb80a26ff26e398c0e4e3fe1fe7f33` on 2026-09-04. The current
  branch changes Workspace ID coverage but not Work polling, item credential,
  heartbeat, Session handling, or Stop ownership.
- Mango adopts the useful lifecycle split: the trusted host owns Poll and Ack;
  one foreground container owns the acknowledged item from first conditional
  heartbeat through Session processing and final Stop; and a per-Session
  workspace persists across multiple activations. The Go SDK supplies the
  provider-neutral item runner and six core local tool executors. Docker only
  supplies process, resource, network, and volume isolation.
- Mango makes the current security guide's per-Session credential path
  mandatory instead of copying the cookbook's broader key handoff or retaining
  the SDK's Environment-key fallback. The Workspace key remains only on the
  supervisor. The opaque per-Work secret is length-framed over a one-shot
  attached stdin stream rather than persisted in Docker environment or command
  metadata. Before reading it, the item runner makes itself non-dumpable on
  Linux; same-UID Bash children therefore cannot use ptrace-gated `/proc`,
  process memory, or descriptors to bypass the scrubbed child environment. The
  container otherwise runs as uid/gid 65532 with a read-only root filesystem,
  all capabilities dropped, `no-new-privileges`, bounded `/tmp`, CPU/memory/PID
  limits, and no Docker socket. Untrusted container logs are not embedded into
  launcher errors.
- Mango changes or defers hosted-specific behavior deliberately. A Workspace
  key temporarily substitutes for CMA's narrower Environment host credential;
  the worker is therefore a trusted Workspace peer. Docker bridge egress is not
  a network policy, and the image is not advertised as a hostile multi-tenant
  boundary. At this slice, Skill, Memory, File/Git input and output preparation,
  server-side Web-tool ownership, persistent Bash/restart behavior, and health
  Work remained explicit gaps rather than fake success paths.
- Acceptance: unit and race tests verify Poll/Ack credential separation,
  one-shot secret framing, container hardening, cancellation-versus-lease-loss
  fencing, bounded tools, path confinement, and credential scrubbing. Opt-in
  real Docker tests prove a Bash child cannot read its parent's `/proc`
  environment, then run two
  Work activations for one Session through Poll and Ack, then cover first and
  subsequent heartbeat, Session SSE, tool result, forced Stop, container
  removal, and workspace-volume continuity. The follow-up persistent-Bash
  slice expands the same real container path to cover state reuse and explicit
  restart. A separate in-flight Bash case
  cancels the host worker and proves that the item posts one error result before
  Stop. The cookbook remains documentation/example input and is not imported
  into Mango's tests.

## Persistent Bash in self-hosted workers

- User problem: coding agents need shell state such as cwd, exported variables,
  and background jobs to survive related tool calls, while timeout or
  cancellation must not leak stale output into the next call. A stateless
  `/bin/bash -c` loop made the worker's coding-tool behavior weaker than the
  lifecycle Mango intended to study.
- Reviewed the public `anthropic-sdk-go`
  [Bash source and tests](https://github.com/anthropics/anthropic-sdk-go/tree/e9c104e7e5fb80a26ff26e398c0e4e3fe1fe7f33/tools/agenttoolset)
  at current `main` commit `e9c104e7e5fb80a26ff26e398c0e4e3fe1fe7f33`
  and the `anthropic-sdk-typescript`
  [Bash input type](https://github.com/anthropics/anthropic-sdk-typescript/blob/4140e0eaa597c0ad35218ffb20b66ef7fce7f639/src/resources/beta/agents/agents.ts#L279-L299)
  at current `main` commit `4140e0eaa597c0ad35218ffb20b66ef7fce7f639`
  on 2026-09-04. Mango adopted the useful public contract: a persistent Bash
  session, optional `restart`,
  per-call `timeout_ms`, combined output, non-zero exit reporting, stdin EOF,
  unpredictable completion framing, bounded output, and automatic replacement
  after timeout, cancellation, shell exit, or framing failure.
- Mango's implementation is independent and self-hosted-specific. It keeps the
  existing Mango field naming outside Bash, strips the `MANGO_*` namespace,
  exposes lifecycle fields only through the self-hosted model schema, and
  treats the Work container rather than the SDK as the isolation boundary.
  Shell state lasts for one Work activation; the Session volume, not the Bash
  process, is the persistence mechanism across activations.
- The open upstream
  [Bash close wedge report](https://github.com/anthropics/anthropic-sdk-go/issues/390)
  supplied failure evidence rather than code: Mango starts process reaping at
  shell creation, kills the process group on reset, and bounds Close waiting so
  a detached descendant or platform failure cannot park Work indefinitely.
  `SessionToolRunner` continues to borrow tools; the launcher-created item
  process owns `CloseAll` and reports cleanup failure.
- Acceptance: SDK race tests cover cwd/environment persistence, restart-only
  and restart-with-command, non-zero exits, stdin EOF, unspoofable framing,
  bounded tail output, timeout/cancellation contamination fences, input
  validation, idempotent close, and interruption of an in-flight command. The
  real Docker Environment Work test executes multiple Bash calls in one
  activation, observes state persistence and restart, then verifies only the
  workspace file survives a second activation.

## Self-hosted Memory Store preparation

- User/operator problem: a self-hosted Session could snapshot a Memory Store in
  the control plane, but the external worker neither materialized that Store nor
  synchronized tool changes. Running anyway would give the model a mount path
  that did not exist and would make execution behavior depend on a
  provider-specific launcher.
- Reviewed the public CMA self-hosted worker and Memory implementations in
  `anthropic-sdk-go` commit
  [`de6914c544629b14a67c0695ce147edae6a291e0`](https://github.com/anthropics/anthropic-sdk-go/tree/de6914c544629b14a67c0695ce147edae6a291e0/lib/environments),
  `anthropic-sdk-python` commit
  [`62de60b27d04f0927a0ccf0f2610597fafcfab6a`](https://github.com/anthropics/anthropic-sdk-python/tree/62de60b27d04f0927a0ccf0f2610597fafcfab6a/src/anthropic/lib),
  and `anthropic-sdk-typescript` commit
  [`ba14b1f4fdf2e840a7b32297965342a099f6201d`](https://github.com/anthropics/anthropic-sdk-typescript/tree/ba14b1f4fdf2e840a7b32297965342a099f6201d/src),
  plus the public self-hosted cookbook at commit
  [`a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5`](https://github.com/anthropics/claude-cookbooks/tree/a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5/managed_agents/self_hosted_sandboxes),
  on 2026-09-05. The three SDKs independently expose the same important
  lifecycle: start heartbeating before one Session fetch; prepare Skills and
  Memory before a per-Session tool factory; expose allowed/read-only roots;
  reconcile on a bounded cadence; run a final sync on clean completion and an
  independently bounded push-only flush on every exit.
- Mango adopts those lifecycle and failure invariants because they solve the
  same self-hosted problem. Store content remains server-authoritative on a
  simultaneous edit; uploads use content SHA-256 preconditions; read-only
  Stores never push; local deletion requires a later corroboration and a
  per-pass cap; and a Store-specific marker prevents an altered directory from
  driving uploads, deletes, or unsafe cleanup. An invalid frozen Store
  declaration is a permanent input failure. Retryable API/transport failures,
  worker-local filesystem failures, and per-Session tool-construction failures
  are retried locally and then left for lease reclaim rather than terminating
  the Session.
- Mango changes the wire and trust details. The worker uses Mango's existing
  `/v1/memory_stores/{id}/memories` contract rather than adding a parallel
  synchronization API. Its per-Work Session token is mandatory, can reach only
  the Stores frozen on that Session, and may mutate only `read_write`
  attachments. Each mutation rechecks the live lease inside the same PostgreSQL
  transaction, closing the authorization-to-reclaim race. There is no
  Environment-key fallback, Anthropic header, beta identifier, hosted
  credential, or runtime dependency on an external SDK.
- Mango keeps the first OSS slice deliberately smaller where behavior does not
  require breadth: Store and file transfers are sequential, the frozen absolute
  mount path has no workdir fallback, and only the first-party Docker launcher
  supplies a `/mnt/memory` filesystem. Read-only roots are enforced by the file
  tools; unrestricted Bash remains inside the Docker security boundary and is
  not presented as a kernel read-only mount. File/Git inputs, other provider
  launchers, retention, and a hostile multi-tenant sandbox are non-goals.
- Acceptance: independently authored HTTP-backed SDK tests cover initial
  download, tool-factory ordering, read-only roots, local/remote conflict,
  periodic delete corroboration, final writeback, and distrust-marker cleanup.
  Middleware and PostgreSQL tests cover attachment scope and stale-lease
  fencing. The opt-in Docker lifecycle test reads and edits an attached Store,
  persists it during teardown, and downloads the updated value in a later Work
  activation without importing a cookbook application into the test suite.

## Docker-default OSS execution (historical, superseded)

This section records the earlier control-plane-managed Docker decision. The
current self-hosted boundary is documented above and deliberately replaces it.

- User problem: the ordinary local deployment must run tools in a separate
  Session container and support Files, Skills, and Memory without manually
  replacing the API or worker. A missing Docker daemon must not permit host
  execution as a fallback.
- Reviewed the public [cloud Environment design](https://platform.claude.com/docs/en/managed-agents/environments)
  and [self-hosted sandbox distinction](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
  on 2026-08-31. Mango retains the useful separation between reusable Environment
  configuration and Session-owned sandbox instances. This is Mango-managed
  execution on the operator's Docker daemon, not a hosted control plane or an
  external Environment Work runner.
- Acceptance: Docker is the binary and Compose default; `local` is not a
  selectable runtime backend; API and worker capabilities agree; the default
  image runs Python; File mounts and outputs are visible across the worker and
  its daemon; restart reattaches the original container; deletion removes it;
  an unreachable daemon fails worker startup. Existing binding and provisioning
  intent semantics remain authoritative, without migrations or an empty-workspace
  fallback for old local bindings.
- Following Docker's [daemon security](https://docs.docker.com/engine/security/)
  and [bind-mount model](https://docs.docker.com/engine/storage/bind-mounts/), the
  development worker is trusted with daemon access and mounts its resource
  directory at the same absolute host/container path. The API receives no daemon
  socket. Session containers receive only their own resource mounts, not the
  socket or worker credentials. This is not a hostile multi-tenant guarantee.
- Non-goals: no external worker, new credentials protocol, provider inventory,
  public API change, automatic local-workspace migration, or cookbook test
  harness. The subsequent test-convergence slice below removes the remaining
  local-based test fixtures.

## Remove host-process sandbox execution (historical, superseded)

This intermediate cleanup preceded the provider-neutral self-hosted worker and
is retained only as design history; no provider binding remains current.

- User/operator problem: a Docker-default binary is insufficient if its tests
  still validate tool behavior through a host-process executor. Remove the
  executor entirely and test the actual execution boundary used by Mango.
- Acceptance: no `LocalProvider`, local provider registration/name, or local-only
  `Spec.WorkDir` remains. Built-in execution and remote-adapter command fixtures
  run in Docker through the Go Engine client. Pure state/protocol doubles never
  launch processes. Required Docker tests fail if the daemon is unavailable;
  offline unit tests never auto-detect or contact it. Examples remain independent.
- Shared output bounding and host-path canonicalization are retained as
  provider-neutral helpers because Docker still requires them. Local-only tests
  are removed; Docker conformance and service tests cover execution, mounts,
  ownership, cancellation, restart, and cleanup.
- This completes the prior OSS execution decision, not a new CMA wire surface.
  No public API, SDK, or database schema changes. Removing the unused WorkDir
  member changes newly computed internal Spec hashes; pre-release deployments
  should drain provisioning intents before upgrading. Existing bound Sessions
  still use their provider reference and package-setup evidence, without a
  legacy-spec translation layer or automatic data deletion.

## SDK resource services (2026-09-07)

Mango's user problem is configuring a reusable Agent team and following its
Session/Thread lifecycle without navigating a flat HTTP operation inventory or
wrapping request fields in transport-only objects. The accepted design and
validation criteria are in [SDK resource design](design/sdk-resources.md).

Paired references: the current public [Agent setup](https://platform.claude.com/docs/en/managed-agents/agent-setup)
and [multiagent orchestration](https://platform.claude.com/docs/en/managed-agents/multiagent-orchestration)
API guides, together with official [Python v1.4.0 resource source](https://github.com/anthropics/anthropic-sdk-python/tree/v1.4.0/src/anthropic/resources/beta),
[TypeScript sdk-v0.124.0 resource source](https://github.com/anthropics/anthropic-sdk-typescript/tree/sdk-v0.124.0/src/resources/beta),
and [Go v1.71.0 source](https://github.com/anthropics/anthropic-sdk-go/tree/v1.71.0).
These are clean-room design references only; none is used to implement or run the
Mango SDK. The unrelated third-party-client research tests were subsequently
removed in the self-hosted boundary follow-up above.

- Adopted resource grouping, direct Python keyword and TypeScript object
  parameters, and language-idiomatic operations (Go New/Get; Python/TypeScript
  create/retrieve). Agent, Session, Thread and Event relationships are preserved.
- Kept Mango's HTTP schemas, generated tagged unions, explicit Optional/null
  values, typed Python dictionaries, and independently implemented transport and
  worker helpers. Go adds constructors for common model, roster and message
  variants; advanced variants remain accessible.
- Adapted nested path IDs to one parent-first positional order across languages.
  TypeScript bracket filters become ordinary identifiers and are encoded back
  into the unchanged HTTP query keys. Its stream method resolves when subscribed;
  raw SSE metadata has a separate lazy iterator. Pagination keeps explicit
  one-page and all-item operations. These choices make request and connection
  ownership visible without hidden writes or reconnection.
- Rejected the hosted beta namespace, Anthropic authentication/headers, model
  catalog restrictions and `ant apply` deployment lifecycle as requirements for
  this SDK slice. File-based deployment management is a separate product problem.
- The Go scoped-client clone reinitializes resource services to use the new
  Session credential. Services share one transport and do not cache remote state.

OpenAPI carries explicit `x-sdk-resource` and `x-sdk-method` metadata, exported
and checked for missing or duplicate mappings. All clients, generators, existing
callers, examples and tests move together; there are no legacy forwarding methods.
Validation uses Mango HTTP conformance, literal payload/routing checks, language
static checks, transport and worker tests, with durability coverage remaining in
its owning Go packages. The source-only alpha 2 packages have not been published.


## API readiness (2026-09-29)

- Mango history: closed, unmerged [PR #57](https://github.com/yanpgwang/mango/pull/57),
  especially commit `e39b182ff19d2374fc09ae509d931be871bb5c82`, supplied the original
  separation of liveness and dependency readiness. The current implementation is
  a smaller slice of that earlier work.
- Adopted: public HTTP liveness, bounded readiness, failure status 503, and
  sanitized errors. Changed: one PostgreSQL pool check also rejects read-only
  transaction mode; it uses Mango's existing error envelope and no probe cache.
- Rejected: hard readiness gates for NATS and Temporal, because asynchronous
  dispatch is backed by Mango's durable PostgreSQL outbox. No hosted-agent API or
  SDK mapping is imported for these self-hosted process probes. First-party SDKs
  retain `system.health` and `system.readiness` with normal API error decoding.
- [Design and acceptance criteria](design/api-readiness.md) distinguish admission
  readiness from later Environment worker execution checks.

## Environment-scoped supervisor credentials

The [Environment credential design](design/environment-credentials.md) records
the API documentation, official Go v1.76.0, Python v1.9.0, TypeScript
sdk-v0.129.0, and cookbook commits reviewed on 2026-09-29. Mango adopts the
standing Environment credential / per-Work execution token separation and
preserves the Environments.Work SDK hierarchy. It replaces Console-only issuance
with its existing database-backed operator CLI, uses standard Bearer auth, and
limits standing keys to Poll, Ack, and Stats for one Environment. Key-row locks
order revocation against Poll/Ack; already-Acked execution retains its independent
lease. These choices protect unrelated Workspace resources while retaining
Mango's self-hosted operation and recovery model. No hosted credentials or
external SDK implementation are required.

## Bounded Environment healthchecks (2026-09-29)

The [healthcheck design](design/environment-healthcheck.md) records paired official
Work API, Go/Python/TypeScript SDK, and self-hosted cookbook references with
reviewed revisions. Mango adopted the Work healthcheck variant and supervisor
versus per-item responsibility split. It omitted a redundant healthcheck ID,
hosted connectivity assumptions, and vendor authentication. Unlike the reviewed
SDK helpers, which skip non-Session items, Mango's Docker worker executes a fixed
local process/filesystem probe and commits a bounded, immutable result. This
serves self-hosted operator verification without a model call or synthetic
Session. The three native clients encode Mango's union and create/result
operations; only the Go helper owns execution. Official implementations were
neither copied nor used as clients or dependencies.

## Session troubleshooting and tutorial retries (2026-09-30)

Reviewed Mango's HTTP projections, OpenAPI error/stop-reason shapes, provider
retry workflow tests, Work lease/reclaim code, and the approval application's
persisted-history reader alongside the current official
[event stream](https://platform.claude.com/docs/en/managed-agents/events-and-streaming)
and [Session operations](https://platform.claude.com/docs/en/managed-agents/session-operations)
guides. Paired official SDK retry-status types were read in Go v1.76.0
(`ad865dfa3d1a8d2f4a7ad0d072011e811e9957a9`), Python v1.9.0
(`a7285e919ab79998d9380b3b57f6315b7860b8d8`), and TypeScript sdk-v0.129.0
(`bf2058689f845dfb10e59bd9ebeb5cb4e9318a9d`).

- Adopted the client responsibility to wait on `retrying` errors and distinguish
  exhausted turns from terminal Sessions. These concepts already exist in
  Mango's API and generated Go SDK; the approval example now uses that mapping
  instead of treating every historical error as fatal.
- Added a [troubleshooting path](guides/session-troubleshooting.md) through
  Mango's existing Session, Event, Thread, and Work APIs, including full
  pagination, action/result reconciliation, and self-hosted lease evidence.
- Kept unknown errors as a stop-and-inspect condition in the bounded tutorial.
  Its one application-owned turn does not automatically submit a new prompt
  after retry exhaustion. Saved decisions and explicit cleanup remain local
  application responsibilities.
- Did not adopt a hosted Console viewer, hosted credential/header rules, or a
  new diagnostics resource. No runtime, HTTP, SDK schema, or database change was
  needed. Official SDKs remain research sources only; validation uses Mango's
  Go SDK and an independently authored application HTTP fixture.

## Self-hosted capability assessment (2026-09-30)

The [refreshed assessment](architecture/self-hosted-workers.md#assessment-on-2026-09-30)
records current official self-hosted, cloud, Vault, MCP, and security documentation,
paired Go v1.76.0 / Python v1.9.0 / TypeScript sdk-v0.129.0 source revisions,
and cookbook `d7265d6ae994ccd8429db0594b000073b2f9ad43`. Mango source and tests at
`7f4e2b5` remain the authority for its current implementation.

- User problem: avoid repeating completed work or prioritizing hosted-only
  conveniences while preparing an independent self-hosted release.
- Durable invariants: protect active uploads during recovery, retain genuine
  crash cleanup, and distinguish historical retry errors from current failure.
  The active-File-upload risk was reproduced only with an application-level
  temporary probe; real PostgreSQL/S3 coverage and a fix remain follow-up work.
- Hosted constraints: managed image inventories, File/Git mounts, hosted output
  lifecycles, and environment-variable secret substitution are not imported as
  self-hosted requirements. Private MCP is not inherently cloud-only; its hosted
  tunnel implementation is not a Mango dependency or required architecture.
- Wire choices: no routes, fields, SDK bindings, or storage schemas changed.
  Useful Work/Session/helper responsibility mappings remain independently owned
  by Mango. The general CMA large-MCP-result documentation does not by itself
  establish the equivalent transport into an external worker.

Earlier follow-up lists now identify delivered healthchecks, credentials, and
the coding/deliverable example instead of presenting them as outstanding work.

## Local object storage (2026-10-03)

Reviewed the official [SeaweedFS 4.48 release](https://github.com/seaweedfs/seaweedfs/releases/tag/4.48),
[Apache-2.0 license](https://github.com/seaweedfs/seaweedfs/blob/4.48/LICENSE),
[pinned mini command](https://github.com/seaweedfs/seaweedfs/blob/4.48/weed/command/mini.go),
[S3 route definitions](https://github.com/seaweedfs/seaweedfs/blob/4.48/weed/s3api/s3api_server.go),
and [S3 API guide](https://github.com/seaweedfs/seaweedfs/wiki/Amazon-S3-API).
The Compose image is pinned to the multi-platform manifest digest
`sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d`.

- Operator problem: the archived MinIO community source required a separate
  image build and checksum-network dependency just to start local and CI tests.
  Adopted the maintained SeaweedFS community image as that stack's default.
- Adopted the single-process development topology, explicit volume sizing, S3
  health probe, and a persistent `/data` containing Filer metadata and bytes.
  Disabled unused Admin UI and WebDAV; only S3 is published to the host.
- Kept Mango's generic S3 adapter, PostgreSQL resource ownership, and existing
  HTTP/SDK contract. Files and Skills stay fully self-hosted. Memory remains in
  PostgreSQL. No hosted agent service, vendor SDK, or enterprise feature is used.
- Did not adopt SeaweedFS as a production requirement, promise complete AWS S3
  parity, or add an automatic MinIO disk/data conversion. Existing state needs
  an explicit transfer or a disposable-stack reset. Namespace metadata and
  object bytes must both be covered by backups.

## Terminal Session connection (2026-10-03)

Reviewed Mango's current Session/Event/Thread HTTP handlers, OpenAPI, event
admission and lifecycle tests, and Go SDK List/Send/Stream mapping alongside the
official [Session terminal connection](https://platform.claude.com/docs/en/cli-sdks-libraries/cli/sessions-connect)
and [events and streaming](https://platform.claude.com/docs/en/managed-agents/events-and-streaming)
guides. The CMA terminal workflow was introduced on September 10. Paired public
Go SDK resource grouping, pagination, Send, and Stream source was reviewed at
v1.78.0 (`c9ebe447ac92c91748af817c265398e5d81ca49f`, released September 30), using
the optional reference checkout's tag rather than its older checked-out HEAD.

- User problem: a self-hosted operator needs to join an existing Session, read
  its history, send messages, and decide pending tool approvals without writing
  a client application. Adopted this workflow and Ctrl+C detachment semantics.
- Durable invariants: open the live stream before paginated history, deduplicate
  durable event IDs, reconcile history after read disconnects, and never retry
  uncertain writes. A second history pass covers the one-time movement of queued
  inputs when `processed_at` is assigned. Idle interrupts preserve pending
  approval barriers. Approval and execution remain separate; relayed child
  approvals are checked against child history and routed by the original action
  ID. Detaching never implicitly interrupts runtime work.
- Hosted constraints: rejected the hosted Console/web viewer, vendor CLI,
  authentication and preview headers, hosted runtime execution, and release
  version requirements. Mango's command connects directly to its operator's API
  with a Workspace key; it does not need model or hosted-service credentials.
- Wire and SDK choices: retained Mango's existing `/v1` Session/Event/Thread
  resources, Go SDK pagination and SSE decoding, and typed message,
  confirmation, and interrupt inputs. The CLI is a line interface with explicit
  `/allow`, `/deny`, and `/interrupt`, not a full-screen UI or sandbox terminal.
  Added `source_event_id` to relayed tool, MCP, and custom actions because Mango
  stores distinct child-local and primary-relay event IDs. This explicit event
  reference lets clients correlate resolutions submitted through either current
  view, without matching arguments, timestamps, or list positions. Go, Python,
  and TypeScript SDK response types preserve it. No database migration or new
  endpoint is needed.

Official implementations were neither copied nor executed as Mango clients.
Validation uses independently authored raw HTTP fixtures through the Mango Go
SDK plus native command tests. PostgreSQL tests verify source references and
resolution routing; HTTP/SDK tests and language typechecks verify the new field.
Runtime execution and admission invariants remain
owned by Mango's existing HTTP, workflow, persistence, and service tests.

## MCP full textual output retention (2026-10-03)

Reviewed the current official [MCP output handling](https://platform.claude.com/docs/en/managed-agents/mcp-connector#mcp-tool-output-handling)
and [self-hosted sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
guides, paired with official Go SDK v1.78.0 (`c9ebe447ac92c91748af817c265398e5d81ca49f`),
`betasessionevent.go` and `lib/environments/worker.go`. These are read-only
clean-room references; no official SDK or cookbook executes as a Mango client.

Adopted the useful lifecycle: MCP output above 100,000 characters gives the
model a 2,000-character preview and a file path it can read later. Mango retains
its projected model-visible UTF-8 text, including structured text, rather than
protocol control metadata or binary contents. This solves lost information in
large log, issue-list, and query results while bounding model context.

Adapted transfer for Mango's independent self-hosted runtime: a completed tool
receipt precedes object storage; a regular Mango File is published under a
stable ID; native Go workers download it before local tool dispatch. The public
`agent.mcp_tool_result.file_id` is a machine-readable Mango adaptation. The
reviewed CMA event type has no equivalent top-level field, and the general CMA
MCP guide does not establish how fallback files reach an external self-hosted
worker. No exact wire parity or confirmed CMA self-hosted transfer is claimed.

Mango limits retained projected text to 32 MiB. It requires configured Files
storage, reports a tool error with a preview if retention is unavailable or
oversized, and retries only File publication after a durable receipt. The
private full byte receipt uses JSON base64 encoding to retain NUL losslessly in
PostgreSQL JSONB, is removed after publication, and never enters Temporal
history, public events, or model context. Generated Files remain pinned while
their Session exists; Session deletion releases them for ordinary File deletion,
preventing replay from resurrecting explicitly deleted output. Images, PDF,
audio, general File mounts, and MCP resources/prompts are outside this slice.

## Shared-storage upload recovery (2026-10-04)

Reviewed the current official [Files workflow](https://platform.claude.com/docs/en/managed-agents/files)
and [Skills workflow](https://platform.claude.com/docs/en/managed-agents/skills),
paired with official Go SDK v1.78.0 (`c9ebe447ac92c91748af817c265398e5d81ca49f`)
`betafile.go` and `betaskill.go`. The API/SDK pair describes upload, immutable
resource reads, and deletion, but does not expose hosted reconciliation or
upload-owner internals. No equivalent CMA internal implementation is claimed.
References were read, not imported or executed.

Mango keeps its existing API/SDK mapping. This work was selected because a
second self-hosted API process could delete another process's live Files/Skills
upload, independently reproduced against PostgreSQL and SeaweedFS. Adopted the
resource visibility invariant: only completed immutable bytes become public;
unknown completion responses cannot justify deleting published bytes.

Mango owns the internal solution: renewable PostgreSQL wall-time leases, atomic
cleanup claims, unique per-attempt Skill archive keys, and periodic recovery.
Independent object cleanup guards are committed with cleanup claims and survive
removal or reuse of upload metadata. Sequence revisions fence stale
acknowledgements; unknown writers retain guards and bounded scans rotate fairly.
Cancellation expires the actual HTTP connection read deadline before closing the
upload source.
No hosted credentials, beta headers, public lease wrappers, new API namespace,
or synchronization obligation are introduced. See the
[recovery design](design/blob-upload-recovery.md) for acceptance and evidence.

## Generated MCP File publication recovery (2026-10-04)

Re-reviewed the official [MCP output handling](https://platform.claude.com/docs/en/managed-agents/mcp-connector#mcp-tool-output-handling)
guide alongside Go SDK v1.78.0 (`c9ebe447ac92c91748af817c265398e5d81ca49f`),
`betasessionevent.go` and `betafile.go`. The paired references establish the
file-backed result workflow and File resource operations, not the hosted
service's object-write transactions or unknown-outcome recovery. No CMA internal
implementation is claimed, imported, or executed.

Kept Mango's existing stable File/event identity, immutable complete-text
receipt, checksum, worker path, and scoped download mapping. The selected Mango
problem was a late S3 request surviving replacement publication or File deletion.
Adapted internal publication to unique physical attempt keys, pre-I/O cleanup
guards, and atomic receipt/key validation. This reuses Mango's existing cleanup
primitive without introducing a public resource or provider-specific wrapper.
Unknown write guards remain persistent because a timeout does not prove that a
remote request cannot commit later. Mango's own PostgreSQL/S3 recovery and
HTTP/SDK tests validate this lifecycle; public third-party schemas do not.


## Automatic tool permissions (2026-10-04)

- Reviewed current official CMA [permission policies](https://platform.claude.com/docs/en/managed-agents/permission-policies)
  and their paired public SDK types: Go `v1.78.0` at
  `c9ebe447ac92c91748af817c265398e5d81ca49f`, Python `v1.11.0` at
  `18f25547f20cf5f01da69ac611e700e3bc9ebf21`, and TypeScript
  `sdk-v0.131.0` at `d49bdab458000bcdffe77bd84b03293f31824fb3`.
  Research included Go `BetaAutoPolicy` and Session evaluation types, Python
  managed-agent auto policy and Session types, TypeScript
  `resources/beta/sessions/events.ts`, and cookbook commit
  `d7265d6ae994ccd8429db0594b000073b2f9ad43` for the surrounding workflows.
  Reference code was not copied, executed, or added as a dependency.
- Mango's user problem is repetitive human approval for routine local/MCP
  calls, while preserving human review when an invocation is uncertain. Adopted
  `auto`, recorded allow/ask/deny, typed event evaluations, ask/indeterminate and
  deny/high_risk reasons, unchanged defaults, denial-as-tool-error, and the
  principle that untrusted tool and inter-Thread text cannot grant authority.
  The first-party SDKs map those wire types coherently; the Go runner consumes
  the recorded outcome rather than reinterpreting the configured policy.
- Mango independently implemented a replaceable, tool-free evaluator using the
  configured Messages adapter, bounded input/output/deadline, strict decoding,
  and fail-safe asking. Its risk criteria and qualification cases are Mango
  design decisions; the reviewed documentation does not disclose CMA's prompt,
  model-selection algorithm, full taxonomy, or judgment quality.
- Adapted execution to self-hosted Work and the existing PostgreSQL/Temporal
  journal. Immutable receipt plus known usage commit atomically; active turn
  ownership independently fences Work publication and MCP dispatch. Original
  client intent is read before compaction; private server-authored task links
  preserve child delegation/report/follow-up provenance. Those links and model
  aliases stay off public DTOs. Budget-paused interruption consumes the old
  round so a budget increase cannot revive it.
- Rejected hosted rollout identifiers, Anthropic authentication/beta headers,
  CMA runtime delegation, and a blanket rule that every consequential action
  must ask. Enabled provider-native Web still requires `always_allow` because
  Mango cannot intercept it before execution. Custom tools remain owned by the
  application. No security-sandbox guarantee, exactly-once provider billing,
  or equivalence to CMA judgment quality is claimed.
- Validation separates raw Mango HTTP and first-party SDK conformance from
  evaluator tests, real PostgreSQL ownership/accounting tests, Temporal tests,
  and a real-service HTTP/worker-restart lifecycle test. See
  [the design](design/automatic-tool-permissions.md) and
  [the user guide](guides/tool-permissions.md).


Automatic-permission qualification refinement (2026-10-04): Mango freezes client
intent privately at child delegation and validates its trigger identity, rather
than deriving a child's authority from mutable processed flags later. Primary
reports include already processed client changes, complete barrier companion
instructions, and terminal outcome descriptions. This is independently designed
self-hosted persistence, not a claim about CMA internals. The configured
`deepseek-flash` endpoint passed 15 authored allow/ask/deny cases after opaque
reasoning was excluded from parsing and the response cap became 1,024 tokens;
20-second timeouts and strict final JSON remain. This supplies limited workload
evidence rather than a general evaluator-quality guarantee.
