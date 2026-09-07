---
title: Mango SDKs
slug: /sdk
---

# Mango SDKs

Use the first-party Go, Python, or TypeScript/JavaScript SDK to avoid repeating
HTTP, multipart, pagination, and SSE code in every application. The client
connects to your Mango server, which continues to own execution, persistence,
scheduling, and sandboxes.

## Packages

| Language | Package / module | Installation and examples |
| --- | --- | --- |
| Go | `github.com/yanpgwang/mango/sdk/go` | [Go SDK](https://github.com/yanpgwang/mango/tree/main/sdk/go) |
| Python | `mango-sdk` (import `mango_sdk`) | [Python SDK](https://github.com/yanpgwang/mango/tree/main/sdk/python) |
| TypeScript / JavaScript | `mango-sdk` | [TypeScript SDK](https://github.com/yanpgwang/mango/tree/main/sdk/typescript) |

Start with the [multi-language quickstart](getting-started.md), or read the
language-specific guides: [Go](sdk/go.md), [Python](sdk/python.md), and
[TypeScript / JavaScript](sdk/typescript.md).

## Current development version

This guide documents the resource-based SDK in the current source checkout:
Python `0.1.0a2` and TypeScript `0.1.0-alpha.2`. These versions are not published.
Use source installation below. The previously published alpha 1 packages have
an earlier client interface; they cannot run the examples on this page.
The [first alpha release record](https://github.com/yanpgwang/mango/blob/main/sdk/releases/0.1.0-alpha.1.md)
remains the record of published artifacts. Go has no independently tagged release.

## Install from source

Use source installation when developing against this checkout, running the
repository examples, or using the Go SDK before a tagged release. Run from the
Mango repository root unless a command says to use your application directory.

```sh tab="TypeScript" tab-group="mango-language"
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run build
# Then, in your application directory:
npm install /absolute/path/to/mango/sdk/typescript
```

```sh tab="Python" tab-group="mango-language"
python3 -m venv .venv
.venv/bin/python -m pip install ./sdk/python
```

```sh tab="Go" tab-group="mango-language"
# In your application module (run go mod init first if needed):
go mod edit -require=github.com/yanpgwang/mango/sdk/go@v0.0.0
go mod edit -replace=github.com/yanpgwang/mango/sdk/go=/absolute/path/to/mango/sdk/go
# Add your Mango import before running tidy.
go mod tidy
```

## Resource services

Services follow the API's resource relationships. They share the client's
transport and credentials; getting a service does not issue an HTTP request.

| Resource | Python / TypeScript | Go |
| --- | --- | --- |
| Agent definitions and versions | `agents`, `agents.versions` | `Agents`, `Agents.Versions` |
| Environments and queued Work | `environments`, `environments.work` | `Environments`, `Environments.Work` |
| Sessions and events | `sessions`, `sessions.events` | `Sessions`, `Sessions.Events` |
| Child Threads and their events | `sessions.threads.events` | `Sessions.Threads.Events` |
| Session attachments | `sessions.resources` | `Sessions.Resources` |
| Files and Skills | `files`, `skills.versions` | `Files`, `Skills.Versions` |
| Memory | `memory_stores` / `memoryStores`, with `memories` and `versions` | `MemoryStores.Memories`, `MemoryStores.Versions` |
| Credentials | `vaults.credentials` | `Vaults.Credentials` |
| Scheduling | `deployments`, `deployment_runs` / `deploymentRuns` | `Deployments`, `DeploymentRuns` |
| Webhooks and public probes | `webhooks`, `system` | `Webhooks`, `System` |

Python accepts request fields as keyword arguments; TypeScript accepts a direct
request object. Go uses request structs and constructors such as `ModelID`,
`Coordinator`, and `UserMessage`. Path identifiers precede parameters in HTTP
hierarchy order: Session ID, then Thread ID, for a Thread's events.

Lists provide one-page methods and explicit all-item iterators. Python uses
`resource.list(...)` / `resource.iter(...)`; TypeScript uses `list`, `listItems`
and `listPages`; Go uses `List` / `ListAutoPaging`.
See [the multiagent SDK applications](guides/multi-agent.md#use-a-first-party-sdk)
for team configuration, completion handling, follow-ups and Thread inspection.

## Authentication and errors

Configure your Mango URL and Workspace API key. Model-provider credentials
stay on the Mango worker, not in the SDK client. Errors expose HTTP status,
Mango error type and request ID for application handling and log correlation.

Optional fields preserve the difference between omission, explicit `null`,
`false`, zero and empty collections. The language README explains its
optional-value representation.

## Events and recovery

The SDKs expose live SSE iteration and persisted event listing. Open a stream
before sending work to observe the new turn. On reconnection, open a new
stream first, list history while it is connected, and deduplicate the sources
by event ID. Preview deltas are ephemeral, not durable history.

The generated one-shot methods do not automatically resend a message or tool
result after an ambiguous HTTP failure. Check persisted events before deciding
to retry. The Go SDK adds three handwritten, provider-neutral workflow helpers:
`WorkPoller`, `SessionToolRunner`, and their `EnvironmentWorker` composition.
The Session runner performs the result-history check before its bounded retry;
it still cannot make an external tool side effect exactly once. The composed
worker adds heartbeat, lease-loss cancellation, scoped credential handoff,
integrity-checked custom Skill preparation, transient retry/reclaim, durable
permanent-input failure, and final Stop without selecting a sandbox provider. See
[Environment Work](api/environment-work.md) and [Session events](api/events.md).
Its optional `tools/agenttoolset` package supplies the six core local tool
executors for an already-isolated worker; it does not create a sandbox.

## Development and verification

```sh
make sdk-install
make sdk-check
make sdk-test
make sdk-conformance
```

`make sdk-generate` regenerates all language bindings from the checked-in
OpenAPI document. Generated-source checks are separate from transport tests.
HTTP conformance exercises real Mango handlers with test-only storage and
model implementations. Service recovery and real-model workflows remain
separate validation tiers.

The SDKs do not depend on the documentation framework. Fumadocs includes named
regions from the runnable quickstart files under each SDK's `examples/`
directory; it does not maintain a second copy of those snippets. `sdk-test`
checks their language types, and `sdk-conformance` runs those exact files against
Mango's HTTP handlers with deterministic test-only repositories and model behavior.

The API reference keeps HTTP routes, request/response schemas, and lifecycle
constraints visible. SDK tabs explain how to invoke that same contract, not a
second contract or a promise that every server capability is production-ready.
