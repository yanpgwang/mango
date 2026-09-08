---
title: SDKs
description: Use Mango from TypeScript, Python, or Go with typed resource clients.
slug: /sdk
---

# SDKs

The first-party SDKs handle authentication, request encoding, pagination, file
uploads, and event streams. They connect to your Mango server through the same
HTTP API documented in the [API reference](api/overview.md).

## Choose a language

| Guide | Runtime | Package / module |
| --- | --- | --- |
| [TypeScript and JavaScript](sdk/typescript.md) | Node.js 22+ | `mango-sdk` |
| [Python](sdk/python.md) | Python 3.11+ | `mango-sdk`, imported as `mango_sdk` |
| [Go](sdk/go.md) | Go 1.24+ | `github.com/yanpgwang/mango/sdk/go` |

For your first complete application, follow the [Quickstart](getting-started.md).

## Current development version

The current resource-based clients are **source-only**: Python `0.1.0a2` and
TypeScript `0.1.0-alpha.2` have not been published, and Go has no independently
tagged release. Install from the same checkout as your server.

The previously published alpha 1 packages use an earlier interface and do not
run these examples. The [release record](https://github.com/yanpgwang/mango/blob/main/sdk/releases/0.1.0-alpha.1.md)
identifies those artifacts. All SDKs remain alpha.

## Install from source

[Clone the repository](getting-started.md#get-the-code) if needed. For
TypeScript, build the SDK in that checkout before installing it in your app.
For Python and Go, run the installation commands from your application's directory.
Replace `/absolute/path/to/mango` with the checkout's actual location.

```sh tab="TypeScript" tab-group="mango-language"
# In the Mango checkout:
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run build

# In your application directory:
npm install /absolute/path/to/mango/sdk/typescript
```

```sh tab="Python" tab-group="mango-language"
python3 -m venv .venv
.venv/bin/python -m pip install /absolute/path/to/mango/sdk/python
```

```sh tab="Go" tab-group="mango-language"
# In an application with a go.mod file:
go mod edit -require=github.com/yanpgwang/mango/sdk/go@v0.0.0
go mod edit -replace=github.com/yanpgwang/mango/sdk/go=/absolute/path/to/mango/sdk/go
# Add your Mango import, then resolve dependencies:
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
See [the multi-agent SDK applications](guides/multi-agent.md#use-a-first-party-sdk)
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

## Contributing to the SDKs

Bindings are generated from Mango's checked-in OpenAPI document. See the
[contributor guide](https://github.com/yanpgwang/mango/blob/main/CONTRIBUTING.md#public-api-changes)
for generation, drift checks, language tests, and HTTP conformance.
