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

## Built-in Agent tools

- The public Agent Toolset shapes and executable cases in the pinned Anthropic
  Go SDK informed Mango's line-oriented `read.view_range` behavior: ranges are
  1-based and inclusive, and a non-positive end reads through EOF.
- Mango retains its existing `path`, `file_text`, `old_str`, and `new_str`
  fields where they remain clear. The public SDK is design evidence, not a
  field-for-field compatibility target or a runtime executor dependency.
- Mango does not advertise `bash.restart` because sandbox commands currently
  execute independently and there is no persistent shell session to restart.
  A future persistent-shell lifecycle must work through Mango's sandbox
  abstraction before that capability can be exposed honestly.
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
support, so they run the same offline and opt-in live conformance suites.

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
  do-observe-fix workflow. A copy of the source license is retained beside the
  test fixture in `internal/temporal/testdata/coding_agent_iterate`.
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
- The [coding-agent iteration example](examples/coding-agent-iterate.md) describes the
  Mango workflow without introducing a second runner. The `internal/temporal`
  scenario test keeps the durable outcome deterministic in public CI; the
  explicit live tier checks the same outcome against a configured model
  endpoint.

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
