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

| Language | Source, local installation and examples |
| --- | --- |
| Go | [Go SDK](https://github.com/yanpgwang/mango/tree/main/sdk/go) |
| Python | [Python SDK](https://github.com/yanpgwang/mango/tree/main/sdk/python) |
| TypeScript / JavaScript | [TypeScript SDK](https://github.com/yanpgwang/mango/tree/main/sdk/typescript) |

Start with the [multi-language quickstart](getting-started.md), or read the
language-specific guides: [Go](sdk/go.md), [Python](sdk/python.md), and
[TypeScript / JavaScript](sdk/typescript.md).

## Install from source

Run these from the Mango repository root. These are local source packages,
not published npm, PyPI, or independently tagged Go releases.

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

The packages cover the current OpenAPI operation inventory, including Memory,
Skills, Files, multi-agent Threads and Environment Work. Coverage means API
access, not an expansion of the [server capabilities](capabilities.md).
These are pre-release source packages; registry releases and a stable SDK
contract have not been established. Install locally as the package README
describes.

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

The SDK does not automatically resend a message or tool result after an
ambiguous HTTP failure. Check persisted events before deciding to retry; an
automatic resend could duplicate work. See [Session events](api/events.md).

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
