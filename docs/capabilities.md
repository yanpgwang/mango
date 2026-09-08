---
title: Capabilities and limits
description: Check the supported scope of a workflow before building on it.
slug: /capabilities
---

# Capabilities and limits

Mango is alpha. There is no supported stable runtime release or production
distribution, and the API may change on `/v1`. The tables below describe current
behavior, not a promise that every combination is production-ready.

- **Supported:** implemented and exercised end to end for the stated scope.
- **Limited:** usable with constraints that affect applications or operations.
- **Preview:** implemented, but broader provider or production evidence is incomplete.

## Choose an execution path

All Environments are `self_hosted`. The first-party launcher runs on Docker;
other launchers are not yet provided.

| Capability | Current behavior |
| --- | --- |
| Text-only model turns and application-owned custom tools | Available without an Environment worker. |
| Shell and file tools | Six core tools; Bash preserves process state within one Work activation. |
| Custom Skills | Immutable pins are downloaded, verified, and prepared before execution. |
| Memory Store attachments | Downloaded and synchronized through scoped Session APIs. |
| File and public Git inputs | Operator stages them in the worker workspace; no automatic control-plane mount. |
| Automatic output publication | Not implemented; workspace files stay with the operator. |
| Web Search / Web Fetch | Run at a supporting model endpoint, with `always_allow`. |
| Remote MCP | Runs through the orchestration runtime. |

The Docker worker's workspace volume persists between activations, but its Bash
process does not. File tools reject writes to read-only Memory roots; Bash
access still depends on the sandbox boundary.

## Capability summary

| Capability | Status | Scope and principal limits |
| --- | --- | --- |
| [Agents](api/agents.md) | Supported | Versioned definitions, immutable Session snapshots, updates, archive, filters, and pagination. |
| [SDKs](sdk.md) | Preview | Go, Python sync/async, and TypeScript resource clients cover the OpenAPI operations. The current alpha 2 interface is source-only; published alpha 1 has an earlier interface. |
| [Sessions](api/sessions.md) | Supported | Persistent work, shared budgets, usage, updates, interrupts, archive, and deletion. Archive retains history; automatic idle reclamation is not implemented. |
| [Events and actions](api/events.md) | Limited | Messages, tool/approval barriers, outcomes, retries, and interrupts. Approval and execution results are separate. File messages support bounded UTF-8 documents, not images or PDFs. |
| [Event streams](api/events.md#stream-events) | Supported | Durable history plus live Session/Thread SSE and optional ephemeral previews. Streams do not replay history or interpret `Last-Event-ID`. |
| [Outcome evaluation](api/sessions.md) | Limited | Text/File rubrics and bounded grading/iteration. The grader does not independently execute tests or inspect output artifacts; validate deliverables separately. |
| [Model and context](architecture/storage-context-and-tools.md) | Limited | Durable transcripts, model-window profiles, usage-based estimates, request admission, and compaction. Exact provider token counts, complete request audit records, and equivalent Outcome/Advisor overflow recovery remain open. |
| [Shell and file tools](guides/self-hosted-worker.md) | Limited | `bash`, `read`, `write`, `edit`, `glob`, and `grep`; `read` is capped at 64 KiB. No host-process fallback. See the execution-path table above. |
| [MCP](architecture/storage-context-and-tools.md#mcp) | Limited | Streamable HTTP, permissions, journaled calls, and Vault bearer/OAuth authentication. Large results are truncated without a full-result file; binary content, private-network connectivity, deprecated SSE, MCP resources, and prompts are unsupported. |
| [Files](api/files.md) | Limited | S3-compatible immutable upload/download, Workspace isolation, and lifecycle recovery. Applications explicitly transfer workspace deliverables. File-sourced images/PDFs and distributed reconciliation remain open. |
| [Session Resources](api/session-resources.md) | Limited | Create-time Memory Store attachments. File/Git staging is operator-owned. |
| [Skills](api/skills.md) | Limited | Validated bundles, immutable Versions, Agent-scoped pins, and instruction loading. External catalogs and repository discovery are not implemented. |
| [Memory](api/memory.md) | Limited | Versioned UTF-8 files, optimistic preconditions, and attached-Store synchronization. Automatic retention and non-Docker self-hosted launchers are not implemented. |
| [Vaults](api/vaults.md) | Limited | Encrypted credentials, Session attachment, OAuth validation/refresh, and rotation. Environment-variable secret egress and refresh-failure notifications are not implemented. |
| [Webhooks](api/webhooks.md) | Limited | Signed Session and Deployment Run lifecycle delivery, with three at-least-once attempts. No delivery-log API or configurable sustained-failure threshold. |
| [Deployments](api/deployments.md) | Limited | Pinned templates, manual and cron runs, Memory Store templates, budgets, leases, and Run records. Agent-archive propagation remains open. |
| [Environment Work](api/environment-work.md) | Limited | Poll/Ack, bounded renewable leases, reclaim, scoped Work credentials, and permanent-input failure. Environment-scoped polling keys and health-check Work remain open. |
| [Multi-agent](guides/multi-agent.md) | Limited | Persistent child Threads, primary-only Advisor consultations, shared budgets, follow-ups, reports, and lifecycle controls. Broader repeated provider evidence and targeted interruption timing remain open. |

## Product and operational boundaries

- **Identity:** Workspace API keys and scoped Work credentials are implemented.
  Your application owns end-user identity and authorization; general roles,
  enterprise key lifecycle, quota, and billing are incomplete.
- **Isolation:** Docker shares the host kernel. The local stack and reference
  worker are not hardened boundaries for hostile tenants. The supervisor has
  Docker daemon authority; network egress policy is operator-owned.
- **Deployment:** the local Compose stack is reproducible, but supported
  production Compose and Kubernetes distributions are not available.
- **Scaling and recovery:** API and orchestration roles can scale independently.
  Worker Versioning, heterogeneous-worker routing, distributed Files
  reconciliation, backup, audit, and observability still need work.
- **Docker ownership:** the operator-run supervisor has Docker daemon authority;
  keep it outside untrusted Session containers and apply ordinary Docker host
  hardening.

Mango owns its API and does not promise drop-in use with a hosted agent service
or third-party SDK. [Product direction](product.md) describes the release and
design policy.

## Verification boundary

Mango's HTTP/SDK tests establish request and response behavior. PostgreSQL,
Temporal, and Docker integration tests cover persistence, recovery, and execution.
Opt-in real-model journeys establish narrower workflow evidence; they do not
establish general production readiness. See
[CONTRIBUTING.md](https://github.com/yanpgwang/mango/blob/main/CONTRIBUTING.md#test-layers-and-ownership)
for the test layers.

Runtime releases will appear in [GitHub Releases](https://github.com/yanpgwang/mango/releases).
Until a supported release exists, match the SDK to the server checkout and use
the [API reference](api/overview.md) for its current contract.
