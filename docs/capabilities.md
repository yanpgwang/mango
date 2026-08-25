---
title: Capabilities and limits
slug: /capabilities
---

# Capabilities and limits

This page inventories Mango's observable API and runtime behavior so operators
can decide whether a workflow is ready for their deployment.

Mango owns its contract and roadmap. The current HTTP resource model retains
ideas and selected surface shapes from public agent-platform specifications.
Mango may continue to reuse or adapt sound routes, schemas, events, and public
SDK types, but the adopted result is Mango-owned and an external service or SDK
does not define future work. See [Product direction](product.md) for the design
and prioritization rules.

Mango has no customers or supported stable release, so there is no
backward-compatibility baseline. All API work changes the existing `/v1`
surface directly. Earlier commits, development databases, and third-party
client behavior are not supported contracts.

Use this page to decide whether a workflow is ready for your deployment:

- **Supported** — implemented and exercised end to end for the stated scope.
- **Limited** — usable with constraints that may affect architecture or
  operations.
- **Preview** — implemented, but live-provider or production evidence is not
  yet strong enough for a support commitment.
- **Not supported** — rejected explicitly rather than silently accepted.

Capability claims are enforced by Mango's HTTP/OpenAPI, PostgreSQL, Temporal,
and service test suites.

## Capability summary

| Capability | Status | Supported scope and important constraints |
| --- | --- | --- |
| Agents and Versions | Supported | Create, get, list, update, immutable Version history, archive, filters, and pagination. Model ID, effort, speed, and `inference_geo` reach working and grader requests. |
| Environments | Supported | Cloud and self-hosted lifecycle, package configuration, limited-network declarations, filters, and pagination. Package execution requires a capable sandbox; limited egress is currently enforced only by OpenSandbox. |
| Sessions | Supported | Create from immutable Agent snapshots, get/list/update/archive/delete, metadata, filters, exact shared public-list-cost budgets, usage, timing, and resource projections. Deletion fences admission and durably releases the Workflow and sandbox. |
| Events and client actions | Limited | System context, messages, thinking, tool events, confirmation/custom/self-hosted result barriers, outcomes, retries, interrupts, and the budget-boundary `session.usage`/`budget_reached` idle sequence are implemented. File-backed message documents are limited to bounded UTF-8 text; File-sourced images and File documents in tool results are not supported. |
| Event streaming | Supported | PostgreSQL-authoritative Session and Thread streams with NATS wakeups, cursor repair, bounded backpressure, and opt-in ephemeral text previews. Streams do not replay history or interpret `Last-Event-ID`. |
| Model and context runtime | Limited | Durable provider-native transcripts, Catwalk-derived model-window profiles with a conservative fallback, provider-usage anchors plus post-anchor estimates, predictive request admission, extractive and oversized-tool-result compaction, one-shot working-turn overflow recovery, and immutable per-Thread turn-preparation checkpoints are implemented. Explicit custom-endpoint overrides, provider-exact counters, complete per-provider-request audit records, later-round projection checkpoints, equivalent Outcome/Advisor overflow recovery, and compaction quality and retention evidence remain open. |
| Sandbox tools | Limited | `bash`, `read`, `write`, `edit`, `glob`, and `grep`, plus provider-native Web Search/Fetch. Local is development-only; Docker and the Preview remote providers expose separately admitted resource capabilities. |
| MCP tools | Limited | Streamable HTTP discovery/execution, permissions, journaled calls, large-result materialization, and Vault bearer/OAuth authentication. Private-network connectivity, deprecated SSE, MCP resources, and prompts are not supported. |
| [Files](api/files.md) | Limited | Configured S3-compatible storage, crash-recoverable intents, reusable snapshotted UTF-8 outcome rubrics, bounded UTF-8 File documents snapshotted into `user.message`, downloadable Session Resource copies, and Docker/E2B/Cube/OpenSandbox/Daytona publication of regular files beneath `/mnt/session/outputs` before idle. Client uploads are intentionally not downloadable. E2B/Cube currently buffer each output archive in worker memory. File-sourced images/PDFs and distributed reconciliation remain open. |
| [Session Resources](api/session-resources.md) | Limited | Independent File copies, create-time Memory attachments, and create-time public HTTPS Git repository snapshots frozen to an exact commit. Runtime File attach/detach works. Git worktrees are writable on Docker/E2B/Cube/OpenSandbox/Daytona and restore offline from Mango storage. Private repository credentials, recursive submodules, LFS objects, repository Skill discovery, and runtime Git attach/detach remain open. Non-Docker Memory mounts are not supported. |
| [Skills](api/skills.md) | Limited | Custom Skill lifecycle, immutable Version pins, strict bundle validation, Docker/E2B/Cube/OpenSandbox/Daytona materialization, Agent-scoped paths, and on-demand instruction injection. Docker is filesystem read-only; remote adapters expose permission-hardened copies and reconcile the main instruction entrypoint. External catalogs, repository sources, and Environment Worker activation remain open. |
| [Memory](api/memory.md) | Limited | Store/Memory/Version lifecycle, immutable history, SHA-256 preconditions, Docker read/write mounts, and deletion-time writeback. Non-Docker mounts and automatic retention are not implemented. |
| [Vaults](api/vaults.md) | Limited | Encrypted Vault/Credential lifecycle, ordered Session attachment, OAuth validation, expiry refresh, and token rotation. Environment-variable egress and refresh-failure notifications are not implemented. |
| [Webhooks](api/webhooks.md) | Limited | Workspace-scoped endpoint CRUD and secret rotation; encrypted Standard Webhooks signing; transactional subscription snapshots; leased at-least-once delivery with three attempts; redirect/private-address auto-disable; Session and scheduled Deployment Run lifecycle events. Delivery logs, an operator-configured sustained-failure threshold, and broader resource events are not implemented. |
| [Deployments](api/deployments.md) | Limited | Deployment/Run lifecycle, pinned Agent Versions, File/Memory/public Git repository templates, per-Run Git resolution and immutable Session snapshots, Session budget templates, manual runs, cron scheduling, leases, and atomic success/failure records. Agent-archive propagation remains open. |
| [Environment Work](api/environment-work.md) | Limited | Self-hosted worker leases, polling, heartbeats, state transitions, reclaim, and Session activation. Environment-key issuance, tenant-scoped authorization, Work secrets, and health-check Work remain open. |
| [Multi-agent](guides/multi-agent.md) | Limited | Persistent ordinary child Agents plus primary-only Mango-managed Advisor consultations over client tool calls, independent transcripts/events/usage, shared Session budgets, reports, routing, interrupts, retries, archive, deletion, and durable context-compaction checkpoints. A real-provider specialist-team journey covers parallel delegation, Advisor consultation, synthesis, and persistent-Thread follow-up; repeated broader live-provider evidence and targeted interruption timing remain open. |
| [Sandbox adapters](sandboxes.md) | Limited / Preview | Local and Docker are available. E2B, CubeSandbox, OpenSandbox, and Daytona have durable bindings, materialize File Resources and custom Skills, and publish Session Outputs through their official SDKs. Remote resource copies have documented limitations; E2B/Cube additionally buffer file transfers. Remote adapters remain Preview pending repeated live conformance and production routing policy. |
| Distributed operation | Limited | API and worker roles scale independently around PostgreSQL, Temporal, and NATS. Worker Versioning, heterogeneous-provider routing, distributed Files reconciliation, and production rollout evidence remain open. |

## Product and operational boundaries

Mango currently has these product and operational boundaries:

- Drop-in interoperability with a hosted agent service or third-party agent SDK
  is not a product goal;
- the API is not stable before the first release;
- the OSS server accepts Workspace-scoped API keys through standard bearer
  authentication and provides tenant data isolation, but not end-user identity,
  roles, per-resource authorization, or enterprise key lifecycle;
- quota, billing, audit, backup, and observability are incomplete;
- Kubernetes and production Compose distributions are not supported;
- local and Docker sandboxes are not safe hostile multi-tenant boundaries.

Unsupported behavior should fail with an explicit validation or capability
error whenever it can be detected at admission time.

## Verification boundary

Capability changes use raw HTTP/OpenAPI tests, PostgreSQL transaction tests,
Temporal replay and integration tests, and real
PostgreSQL/Temporal/NATS/MinIO/Docker service tests.

Mango has not published a versioned release. Published versions will appear in
[GitHub Releases](https://github.com/yanpgwang/mango/releases). Follow Mango's
[API reference](api/overview.md) for the current wire surface, this page for
operational boundaries, and current
[pull requests](https://github.com/yanpgwang/mango/pulls) or optional
[Issues](https://github.com/yanpgwang/mango/issues) for active work.
