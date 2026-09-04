---
title: Design provenance
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

## HTTP transport

- Claude Managed Agents documentation and public SDK behavior informed early
  use of `x-api-key`, provider version and beta headers, and a provider-named
  worker correlation header.
- Mango uses standard `Authorization: Bearer` authentication and media types,
  retains the generic `request-id` response header, and exposes optional worker
  correlation as the `worker_id` query parameter. It does not expose provider
  rollout headers on its inbound API.
- The Anthropic Messages adapter continues to send the provider headers its
  outbound endpoint requires. Tests that exercise Mango through an Anthropic
  SDK are optional research evidence; raw HTTP and OpenAPI tests define Mango's
  transport contract.
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
- `github.com/anthropics/anthropic-sdk-go` at the version pinned in `go.mod`
  supplied request and response examples during early development. It is not a
  runtime dependency, compatibility baseline, or authority over Mango's API.

Mango's bounded UTF-8 projection, private admission snapshot, S3-compatible
storage, and explicit rejection of multimodal File sources are local design
choices documented in [Files](api/files.md) and
[capabilities and limits](capabilities.md).

## File-backed Session Resources

- The [Managed Agents Files guide](https://platform.claude.com/docs/en/managed-agents/files)
  defines independently copied File resources, their read-only presentation
  beneath `/mnt/session/uploads`, optional mount paths, and runtime add/delete.
- `github.com/anthropics/anthropic-sdk-go` at the version pinned in `go.mod`
  supplied Session Resource request and response examples during early
  development. Existing tests using those types may change or be removed with
  Mango's `/v1` design.
- Remote File Resource behavior is implemented against pinned provider Go
  clients. The [OpenSandbox Go SDK](https://github.com/alibaba/OpenSandbox/blob/main/sdks/sandbox/go/README.md)
  and [Daytona filesystem guide](https://www.daytona.io/docs/file-system-operations/)
  define streaming upload/download, metadata and permission operations,
  directory management, and move/delete. The
  [CubeSandbox Go SDK](https://github.com/tencentcloud/CubeSandbox/tree/master/sdk/go)
  supplies the E2B/Cube-compatible whole-value file operations. These provider
  APIs are implementation dependencies rather than definitions of Mango's
  target contract.

Mango's provider-owned marker format and retry algorithm are independent local
design choices documented in [Sandbox backends](sandboxes.md). Remote adapters
intentionally stop at writable sandbox-local copies in the current
implementation. E2B and Cube additionally accept whole-file worker buffering
until their pinned Go data plane exposes streaming operations. These limitations
are documented as Mango behavior rather than inferred from provider APIs.

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

- Anthropic's public
  [`CMA_iterate_fix_failing_tests` cookbook](https://github.com/anthropics/claude-cookbooks/blob/main/managed_agents/CMA_iterate_fix_failing_tests.ipynb)
  supplied the MIT-licensed `calc.py` and `test_calc.py` fixture and the useful
  do-observe-fix workflow. System tests own the fixture under
  `internal/temporal/testdata/coding_agent_iterate`; the standalone example owns
  separate inputs under `examples/coding-agent/fixtures`. Both retain the source
  license. The example adapts the checks to standard-library `unittest` so it
  needs no sandbox package installation; the original assertions are retained.
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
  the [coding-agent example](examples/coding-agent-iterate.md) into a runnable
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
- The example's expense flow is a runnable public-HTTP program exercised against
  a real model. Its client represents the external expense system and prompts
  a terminal user for the review decision, while the deterministic scenario
  separately proves crash and concurrency invariants.
- Mango changed the hosted presentation behavior. One idle event exposes every
  action in the current barrier rather than a sliding window. Partial results
  are durably claimed without waking execution; the final result resumes the
  complete result round exactly once, including after worker replacement.
- The scenario uses Mango-owned synthetic inputs and copies no Cookbook
  fixture. Its executable contract is PostgreSQL atomic admission, Temporal
  recovery, duplicate-result rejection, and persisted Event ordering.
- Mango's Webhook slice can now wake the application on
  `session.status_idled`, but the example retains stream-plus-history recovery:
  an at-least-once notification is not the authoritative custom-tool barrier.

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
  Skills and Environment Worker activation remain separate product decisions.

## OSS execution capability admission

- Reviewed the public [CMA self-hosted sandbox guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
  and [Skills guide](https://platform.claude.com/docs/en/managed-agents/skills)
  on 2026-08-31. CMA's external worker helpers prepare a workspace and activate
  pinned Skills; the existence of a Work API alone does not supply that runtime.
- Mango's user problem is narrower: a Session must not be admitted with a
  statically known capability mismatch and fail only after execution starts.
  Mango keeps version-pinned Skills but rejects self-hosted Session creation
  when the effective primary Agent or any resolved roster member has Skills.
  Overrides apply before admission, including to `self` roster copies.
- The rejection precedes Session persistence, Work activation, and execution
  wakeups. Deployment Runs reuse the same admission path, record the existing
  `session_creation_rejected_error`, and pause a scheduled Deployment for an
  unsupported capability. Manual Run failures do not pause it. Replaying a
  scheduled occurrence returns the same immutable failure record.
- Acceptance is verified through Mango HTTP and isolated PostgreSQL tests:
  inherited, pinned, override-added, and roster-only Skills are rejected;
  clearing effective Skills permits an otherwise valid self-hosted Session;
  capable Mango-managed sandboxes retain Skill admission. No hosted agent
  credentials, vendor SDK worker, new authentication scheme, or migration is
  required.
- This slice also clarifies existing limits rather than changing their
  semantics: Outcome grading is tool-free evidence evaluation, archive does
  not release a sandbox, and Environment Work is a protocol without a
  first-party runner. Artifact verification, SDK workflow helpers, sandbox
  reclamation, and OSS cost accounting are separate delivery slices.

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
  Heartbeat/Stop, the exact Session's read/event routes, and File/Skill inputs
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
  Neither path creates
  compute, prepares File/Git/Skill/Memory inputs, closes tools, or introduces a
  provider SDK. Environment-scoped Poll credentials remain a future requirement
  before supervisors are described as untrusted or multi-tenant.
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

## Docker-default OSS execution

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

## Remove host-process sandbox execution

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
