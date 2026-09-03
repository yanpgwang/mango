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

The public Claude cookbook at commit
`26b5cdce81d357596f5df7f44f50908a80be40cf` and `anthropic-sdk-go` v1.69.0
at commit `6298207eac7ff589e7fcc8a78f6c034ab09de47f` were reviewed on
2026-09-03. The cookbook reference set is Docker, Cloudflare Containers, a pure
Cloudflare Worker variant, Modal, Daytona, and Vercel. Those implementations
confirm the separation above: the compute platforms expose generic container,
process, filesystem, or volume primitives; Anthropic's SDK/CLI worker implements
the managed-agent protocol.

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

## Lifecycle and security invariants

- Poll is a tentative claim; Ack must complete before execution is handed off.
- A worker that loses the heartbeat lease stops executing and must not submit a
  successful result afterward.
- Re-delivery and process restart are normal. Event IDs and persisted pending
  actions, not in-memory seen sets, determine whether a result is outstanding.
- A Work poller does not Stop acknowledged items. The composed
  `EnvironmentWorker` owns heartbeat and final Stop while `SessionToolRunner`
  owns only the Session event/tool loop; an ambiguous Ack or invalid Ack
  response is left for TTL reclaim.
- Poll rotates an unpredictable per-claim `sessions_token` inside the Work
  secret payload. The worker switches to it for heartbeat, Stop, Session events,
  and pinned immutable inputs after Ack; reclaim invalidates the old token and
  closes an established event stream. Lease TTL is capped at five minutes, and
  a graceful Stop can retain execution access for no longer than that current
  TTL. Ack continues to use the supervisor credential and every non-Poll Work
  response redacts the payload.
- The Session credential may submit tool results only. It cannot manufacture
  user input, approval decisions, or persistent system context.
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
4. Compose those pieces as `EnvironmentWorker` and add scoped Environment
   polling credentials before claiming an untrusted or multi-tenant supervisor
   boundary.
5. Build a standalone Docker launcher and run a real Docker end-to-end flow.
6. Add thin provider examples one at a time. Each must use the same runner and
   document persistence, cancellation, resource limits, network policy, and
   restart behavior.
7. Remove the old Mango-managed `cloud` Environment path and compiled provider
   registry only after the Docker worker replaces their observable OSS
   workflow. Mango is pre-release, so the final API change happens directly on
   `/v1` without a compatibility layer.

These SDK slices deliberately leave current execution intact. They establish
the worker-side protocol without coupling examples to Temporal or claiming that
a poller or tool runner alone is a complete sandbox integration.
