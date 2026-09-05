---
title: Self-hosted workers
---

# Self-hosted workers

Mango is converging on one provider-neutral execution boundary: the control
plane assigns a Session through Environment Work, and an operator-run worker
executes tools in infrastructure selected by that operator. Docker, Daytona,
Modal, Cloudflare, and Vercel are worker deployment choices, not Environment
types and not control-plane provider values.

```text
Mango control plane
  Work queue + Session events + tool results
                 |
Mango SDK worker layer
  WorkPoller + EnvironmentWorker + SessionToolRunner
                 |
Operator launcher
  Docker / Daytona / Modal / Cloudflare / Vercel
                 |
Generic sandbox infrastructure
```

## Product decision

The target OSS product supports `self_hosted` execution. Mango does not plan an
Anthropic-style `cloud` Environment in which the Mango project operates a
shared hosted sandbox fleet for its users. An operator may choose a commercial
compute or sandbox service for their worker; that remains self-hosted from
Mango's trust-boundary perspective because the operator owns the account,
credentials, policy, and lifecycle.

The control plane must not import provider SDKs or expose provider-specific
fields on Environment or Session resources. A provider example may use a
generic API to create a sandbox, upload the same Mango runner, start it with the
claimed Work identity, and tear it down. The runner, not the provider, owns the
Mango protocol: event recovery, permission gates, heartbeat, lease loss, tool
result submission, and Stop.

## Reference scope

The public Claude cookbook at `main` commit
`a97b9a2dc300635f0c26b5e05d0b54bbe0279ee5` and the current public Go, Python,
and TypeScript SDK sources at commits `de6914c544629b14a67c0695ce147edae6a291e0`,
`62de60b27d04f0927a0ccf0f2610597fafcfab6a`, and
`ba14b1f4fdf2e840a7b32297965342a099f6201d` were reviewed on 2026-09-05.
The cookbook reference set is Docker, Cloudflare Containers, a pure Cloudflare
Worker variant, Modal, Daytona, and Vercel. Those implementations confirm the
separation above: compute platforms expose generic container, process,
filesystem, or volume primitives; the provider-neutral SDK/CLI worker owns the
managed-agent protocol and Session Memory synchronization.

Mango will use the same separation without treating that list as a provider
compatibility promise:

- Docker is the first required end-to-end reference and the OSS development
  default.
- Daytona, Modal, Cloudflare Containers, and Vercel are independent launcher
  examples after the shared runner exists.
- The pure Cloudflare Worker example is useful for the lower-level runner API,
  but its in-isolate fake filesystem is not equivalent to a Linux sandbox and
  must not be advertised as one.
- E2B, CubeSandbox, and OpenSandbox are outside this cookbook-alignment slice.
  A future example needs its own user/operator reason rather than inheriting an
  old compiled adapter.

Examples stay outside contract and system-test harnesses. Credential-free unit
tests cover shared worker behavior; provider live checks remain explicit and
opt-in.

## Current behavior alignment

| Concern | Public CMA behavior | Mango behavior | Status |
| --- | --- | --- | --- |
| Claim boundary | A trusted host Polls and Acks | The Docker supervisor Polls and Acks | Aligned |
| Item credential | Work can carry a per-Session secret, which the SDK prefers; the SDK can fall back to the Environment key and the Docker cookbook currently passes that broader key into the container | The scoped Work secret crosses one-shot stdin into a non-dumpable item runner; it is absent from Docker environment and command metadata, while the Workspace key stays on the supervisor | Same scoped normal path; intentionally stricter fallback and child-process boundary |
| Lease ownership | The item runner performs first heartbeat, continuous renewal, Session handling, and Stop | `EnvironmentWorker.HandleItem` owns the same sequence | Aligned |
| Workspace continuity | Docker examples retain a per-Session workspace across activations | A named `/workspace` volume is keyed by Session ID and retained after each Work container exits | Aligned |
| Agent tools | Core shell/file tools execute inside customer infrastructure; Web tools remain server-side | The Docker image executes `bash`, `read`, `write`, `edit`, `glob`, and `grep`; Web Search/Fetch use the configured model endpoint and never become external result waits | Same execution boundary; native Web endpoint support and `always_allow` are current Mango requirements |
| Shell lifecycle | The current SDK keeps a persistent Bash process and supports restart/per-call timeout | The self-hosted Go toolset keeps one PTY-backed Bash per Work container, exposes `restart` and `timeout_ms`, and replaces the shell after timeout, cancellation, or framing failure | Aligned lifecycle; Mango additionally bounds shutdown reaping |
| Session inputs | The public worker prepares supported Skill and Memory state before execution | The Go worker prepares immutable primary/roster Skill bundles and attached Memory Stores before constructing per-Session tools; File/Git preparation remains open | Aligned for Skills and Memory |

This table is a behavioral audit, not a compatibility claim. CMA's current
security guide recommends passing a Work item's per-Session secret only to that
Session sandbox, while its SDK retains an Environment-key fallback. Mango makes
the narrower path mandatory: it will not pass a Workspace credential into a
Session container merely to copy the cookbook script.

## Lifecycle and security invariants

### Web execution ownership

A self-hosted Session must be able to use the configured model endpoint's Web
Search/Fetch alongside the six sandbox tools. The Environment selects where
shell and file operations run; it does not move provider-native Web tools into
the external worker. The current Messages adapter already supports those Web
tools and preserves their opaque responses in the durable model transcript.

Acceptance criteria for this slice:

- Both Environment paths declare enabled Web tools as provider-native tools.
  Self-hosted Bash retains its persistent-shell contract.
- Only shell/file and custom calls may request an external result. Provider Web
  calls never create an external pending-action barrier; a malformed ordinary
  client call to a provider-owned Web tool is rejected before tool execution.
- Web tools retain the existing `always_allow` restriction. The current model
  adapter cannot suspend a provider-native call for a Mango approval.
- Tests verify mixed Web/sandbox requests, correlated external results,
  lossless Web transcript recovery, and failures without a sandbox acquisition.

This slice does not add a Web provider, new model credentials, domain filters,
an external worker Web executor, or automatic File/Git transfer. Endpoints that
do not support the current native Web declarations must use agents with those
tools disabled. Full Docker/control-plane recovery and the default deployment
cutover follow separately.

### Work lifecycle

- Poll is a tentative claim; Ack must complete before execution is handed off.
- A worker that loses the heartbeat lease stops executing and must not submit a
  successful result afterward.
- Normal worker shutdown cancels an executing tool, gives its error result a
  bounded independent send window, then Stops the Work. Lease loss remains a
  hard fence: the old owner cancels locally without submitting any result or
  issuing Stop.
- Re-delivery and process restart are normal. Event IDs and persisted pending
  actions, not in-memory seen sets, determine whether a result is outstanding.
- A Work poller does not Stop acknowledged items. The composed
  `EnvironmentWorker` owns heartbeat, permanent-input Fail, and final Stop while
  `SessionToolRunner`
  owns only the Session event/tool loop; an ambiguous Ack or invalid Ack
  response is left for TTL reclaim.
- Poll rotates an unpredictable per-claim `sessions_token` inside the Work
  secret payload. The worker switches to it for heartbeat, Fail, Stop, Session
  events,
  and pinned immutable inputs after Ack; reclaim invalidates the old token and
  closes an established event stream. Lease TTL is capped at five minutes, and
  a graceful Stop can retain execution access for no longer than that current
  TTL. Ack continues to use the supervisor credential and every non-Poll Work
  response redacts the payload.
- The Session credential may submit tool results only. It cannot manufacture
  user input, approval decisions, or persistent system context.
- The same credential may read only Memory Stores attached to its frozen
  Session, and may mutate only `read_write` attachments. Every Memory mutation
  rechecks the live Work lease in the same PostgreSQL transaction as the write,
  so reclaim cannot race a previously authorized request.
- No model-provider key or broad server credential belongs in a sandbox.
- Mango currently authorizes supervisor Poll/Ack with a Workspace API key and
  item execution with the per-Session token. Until scoped Environment polling
  credentials exist, worker supervisors are trusted peers within the Workspace;
  only the item token may cross into the Session sandbox.

## Incremental delivery

1. Added a Mango Go SDK `WorkPoller` over the existing Work endpoints. It handles
   poll, Ack, drain, reclaim, and cancellation, but neither stops Work, creates
   sandboxes, nor executes tools.
2. Added the CMA-shaped Work secret payload and per-Session bearer scope.
   Heartbeat, Stop, Session event execution, and pinned immutable inputs can use
   the item token; reclaim invalidates it before another worker starts.
3. Added a provider-neutral single-Session `SessionToolRunner`. It connects the
   stream before paginated history reconciliation, dispatches local and custom
   tools, honors durable confirmation gates, retries ambiguous result writes,
   and stops on terminal events, end-turn idle, cancellation, or lease loss.
4. Added a provider-neutral `EnvironmentWorker` composition. It performs a
   successful conditional heartbeat before tool execution, keeps heartbeating
   in parallel, derives result retry bounds from the lease TTL, cancels on
   lease loss, and force-Stops ordinary exits. It requires the per-Work token
   after Ack and never falls back to the supervisor's Workspace key. Scoped
   Environment polling credentials remain required before claiming an
   untrusted or multi-tenant supervisor boundary.
5. Added a standalone Docker launcher. A trusted supervisor polls and Acks,
   creates one hardened container per Work item, gives it only the Work secret,
   and retains one named workspace volume per Session. A real Docker test covers
   Poll through Stop, Skill preparation before dispatch, and a second activation
   reading both its re-prepared Skill and the first activation's persistent file.
6. Added the persistent self-hosted Bash lifecycle. One PTY-backed shell keeps
   cwd, environment variables, and background jobs within an activation;
   explicit restart, per-call timeout, cancellation recovery, bounded output,
   and bounded shutdown are SDK-owned rather than Docker-specific.
7. Added provider-neutral Memory preparation. The worker downloads attached
   Stores before constructing tools, exposes writable and read-only roots,
   reconciles with SHA-256 preconditions after tool calls, performs a final
   sync or cancellation-safe push-only flush, and removes only trusted folders
   it created. Docker supplies a bounded `/mnt/memory` tmpfs; no provider logic
   enters the SDK lifecycle.
8. Keep Web Search/Fetch on the model endpoint in both Environment paths. Only
   the six shell/file tools belong to the self-hosted built-in result protocol.
   Provider responses and error blocks survive external-result waits and
   orchestration-worker restart in the durable transcript.
9. Verify the complete Docker worker against the real control plane and backing
   services, including recovery, lease loss, approvals, Skills, and Memory.
   The existing real-Docker launcher test uses an HTTP control-plane fixture;
   separate PostgreSQL/Temporal tests cover server invariants.
10. Switch the default deployment, quickstart, and SDK examples to self-hosted
   execution, then remove the old `cloud` path and compiled provider registry.
   Record the resulting File/Git and output boundary explicitly. Mango is
   pre-release, so this changes `/v1` directly without a compatibility layer.
11. Add thin provider examples one at a time. Each must use the same runner and
   document persistence, cancellation, resource limits, network policy, and
   restart behavior.

CMA's self-hosted guide rejects File/Git resource mounts and leaves input
staging and deliverable retrieval to the operator. Its SDK worker prepares
Skills and Memory, not automatic File/Git inputs or output publication. Mango
may add those conveniences for an independently selected user workflow, but
they are not CMA self-hosted parity requirements or mandatory prerequisites to
the default deployment cutover. Until supported, the API continues to reject
self-hosted File/Git attachments explicitly.

A future operator-managed sandbox service may use the same Work and runner
boundary, with a Mango-managed launcher owning provisioning and reclamation.
That possibility does not require retaining the current provider registry or
adding an unused cloud abstraction now.

The self-hosted worker path still coexists with the legacy Mango-managed
`cloud` path. The remaining steps above are the explicit convergence plan; the
current implementation must not be described as conceptually complete until
that path and its provider registry are removed.
