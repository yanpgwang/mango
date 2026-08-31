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

The packages cover the current OpenAPI operation inventory, including Memory,
Skills, Files, multi-agent Threads and Environment Work. Coverage means API
access, not an expansion of the [server capabilities](capabilities.md).
These are alpha SDKs; a stable SDK contract has not been established. The package
READMEs document exact-version registry installation for published versions
and local installation from this checkout. Release preparation is tracked in
the [release guide](https://github.com/yanpgwang/mango/blob/main/sdk/RELEASING.md).
Package metadata does not by itself confirm a registry release. Match the SDK
version to the server revision you deploy.

The TypeScript/JavaScript alpha is published on
[npm](https://www.npmjs.com/package/mango-sdk/v/0.1.0-alpha.1):

```sh
npm install mango-sdk@0.1.0-alpha.1
```

The Python alpha is published on
[PyPI](https://pypi.org/project/mango-sdk/0.1.0a1/). With Python 3.11 or newer,
install it in a virtual environment:

```sh
python -m pip install 'mango-sdk==0.1.0a1'
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

The SDKs and code samples do not depend on a documentation-site framework.
A future documentation migration can reuse the OpenAPI document and samples
without changing the server or clients.
