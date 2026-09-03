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

## Install an alpha

The packages cover the current OpenAPI operation inventory, including Memory,
Skills, Files, multi-agent Threads and Environment Work. Coverage means API
access, not an expansion of the [server capabilities](capabilities.md).
These are alpha SDKs; a stable SDK contract has not been established. Match the
SDK version to the server revision you deploy.

Python and TypeScript/JavaScript are published on
[PyPI](https://pypi.org/project/mango-sdk/0.1.0a1/) and
[npm](https://www.npmjs.com/package/mango-sdk/v/0.1.0-alpha.1). Install by exact
version; Go has no independently tagged release yet and uses source installation.

```sh tab="TypeScript" tab-group="mango-language"
npm install mango-sdk@0.1.0-alpha.1
```

```sh tab="Python" tab-group="mango-language"
python3 -m venv .venv
.venv/bin/python -m pip install 'mango-sdk==0.1.0a1'
```

The TypeScript package includes compiled JavaScript and declarations; consuming
it does not require building the SDK. The alpha is not a stable release even if
npm displays it under `latest`. See the
[release record](https://github.com/yanpgwang/mango/blob/main/sdk/releases/0.1.0-alpha.1.md)
for verified artifacts and the
[release guide](https://github.com/yanpgwang/mango/blob/main/sdk/RELEASING.md)
for the publishing process. Package metadata alone is not evidence of publication.

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
to retry. The Go `SessionToolRunner` is a narrow workflow helper that performs
that result-history check before its bounded retry; it still cannot make an
external tool side effect exactly once. See [Session events](api/events.md).

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
