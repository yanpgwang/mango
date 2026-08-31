# Mango SDKs

First-party clients for Mango's own HTTP API. They connect to your Mango
server; they do not run an Agent loop or call a hosted agent service.

| Language | Source and local setup |
| --- | --- |
| Go | [Go SDK](go/) — standalone module, standard-library transport |
| Python | [Python SDK](python/) — synchronous and asynchronous clients |
| TypeScript / JavaScript | [TypeScript SDK](typescript/) — native fetch and async iteration |

The npm package [`mango-sdk@0.1.0-alpha.1`](https://www.npmjs.com/package/mango-sdk/v/0.1.0-alpha.1)
is published. Install it with `npm install mango-sdk@0.1.0-alpha.1`.
The Python `mango-sdk` candidate is built and tested but is not yet published
to PyPI; use its local installation instructions for now.

All three target every operation in the current OpenAPI document: Agents,
Environments, Environment Work, Sessions, Events, Threads, Resources, Files,
Skills, Memory, Vaults, Webhooks, Deployments and public diagnostics. An SDK
method does not remove a server capability restriction.

Python and TypeScript use the distribution name `mango-sdk`. Python imports
`mango_sdk`; TypeScript imports `mango-sdk`. Their initial release candidates are
`0.1.0a1` (Python) and `0.1.0-alpha.1` (npm, under the `alpha` dist-tag).
The Go module remains `github.com/yanpgwang/mango/sdk/go`, without an independent
version tag. Source installation remains available for every language.
There is no third-party SDK compatibility promise or independently stable SDK
API. See [release preparation and verification](RELEASING.md) and the
[first alpha release record](releases/0.1.0-alpha.1.md).

## One contract, three packages

The repository-wide commands need the existing Go toolchain, Python 3.11+,
Node.js 22+ and [uv](https://docs.astral.sh/uv/). CI pins uv to 0.12.5; Python
and npm dependency lockfiles keep package development installs reproducible.

`internal/httpapi/openapi.yaml` is the source of truth. The offline exporter in
`scripts/sdk-contract` produces `sdk/openapi.json` with unchanged schemas and
`sdk/operations.json` as a resolved operation index. Each language owns a small
deterministic binding generator and its transport. Generated source is checked
in so packages build independently; drift checks reject stale bindings.

```sh
make sdk-install       # isolated Python environment and npm development deps
make sdk-generate      # export contract and regenerate language bindings
make sdk-check         # reject stale snapshots or bindings
make sdk-test          # language tests, type checks and builds
make sdk-conformance   # clients against the real Mango HTTP router
```

The conformance tier uses test-only repositories and a model fake behind
Mango's real handlers. It checks request serialization, response decoding,
pagination and errors without credentials or billable calls. It is not
live-model, PostgreSQL, Temporal or Docker evidence; those retain their
existing service and opt-in live test tiers.

## Common behavior

- Standard bearer authentication; no vendor beta headers or hosted credentials.
- Explicit base URL selection; no hosted-service discovery.
- Named, typed operations generated from Mango's OpenAPI operation IDs.
- Omitted values remain distinct from explicit `null`, `false` and empty lists.
- Finite-request timeouts and cancellation. Close streams when no longer needed.
- Errors retain HTTP status, Mango error type and request ID.
- No automatic replay of mutations. A timeout does not prove a write failed.
- Redirects are not followed with the API credential.
- Pagination handles opaque `next_page` cursors and Files' ID cursors.

SSE streams are **live-only**. They do not implement `Last-Event-ID`, automatic
exactly-once delivery or durable consumer storage. To reconnect without missing
persisted events, open a stream, list history, merge both and deduplicate by
event ID. Text previews are ephemeral. See [the event contract](../docs/api/events.md).

These packages are API access layers. New worker credential scopes, billing,
an Agent runner and a workflow DSL are not SDK features. Generation, builds and
tests do not need a SaaS code-generation account.
