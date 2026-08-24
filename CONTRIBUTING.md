# Contributing to mango

Thanks for helping improve the project. Mango is an independent, self-hosted
runtime with its own product contract. Some of its original resource and wire
design was informed by public agent-platform specifications. Their routes,
resource models, JSON shapes, event types, and public SDK types are legitimate
design starting points when they fit Mango and avoid unnecessary invention.
Changes must still preserve a clear line between external design research and
Mango's original implementation and product decisions.

## Before a substantial change

Describe the following in the pull request, a design document, or an Issue:

- the user-visible problem;
- the Mango user or operator rationale and any relevant design references;
- the durability, retry, and security implications;
- a small independently testable delivery slice.

Issues are optional coordination and roadmap tools, not an implementation gate.
For solo development and short-lived work, putting this context directly in the
pull request is preferred over opening an Issue that will immediately close.
Use an Issue when work benefits from discussion, sequencing, ownership, or
longer-term tracking. Small bug fixes and documentation improvements can go
directly to a pull request with proportionate context.

## Development setup

Requirements:

- Go 1.26 or newer;
- [golangci-lint](https://golangci-lint.run/docs/welcome/install/local/)
  2.12.x for local lint checks;
- Node.js 20 or newer for the documentation site;
- Docker with Compose for service-conformance and Docker sandbox tests.

Run the core checks:

```bash
make verify
```

`make lint` checks changes relative to `origin/main`, matching the incremental
CI rollout. Set `LINT_BASE` when your comparison branch differs.

Run the documentation checks:

```bash
make docs-check
```

Run reachable Go vulnerability scanning and fail on high-severity production
dependency advisories for the documentation toolchain:

```bash
make security
```

Validate the deployment configuration and container entrypoint:

```bash
make local-config
make image-smoke
```

Run the same PostgreSQL, Temporal, NATS, MinIO, and Docker conformance suite as
CI:

```bash
docker compose -f deployments/local/compose.yaml up -d --wait postgres temporal nats minio
make test-service
```

Default tests must stay offline and deterministic. Service tests must use
isolated database schemas and clean up their workflows, File objects, and
sandboxes. A real model endpoint is a separate, explicitly enabled test tier
because it uses a credentialed network call and may incur cost:

```bash
make test-model-live
make test-platform-live
scripts/with-dev-env make test-coding-agent-live
```

The live targets require the `MANGO_MODEL_*` variables documented in
the getting-started guide. They are intentionally not run in public CI and must
never print or persist API keys.

The deterministic form of the coding-agent scenario runs in the ordinary
service suite and can also be selected directly with `make test-coding-agent`.
Keep its documented example, fixture, and deterministic/live outcome assertions
aligned.

The durable custom-tool gate scenario can be selected with
`make test-hitl-gate`; its credentialed user journey runs with
`scripts/with-dev-env make demo-hitl-gate` against the public HTTP API. Keep
its documented example aligned with the complete-barrier, partial-result,
duplicate-result, worker-replacement, and interactive live-model assertions.

Cookbook-derived examples describe real Mango user journeys, not probe-only
demos. Keep an offline deterministic test for exact runtime and recovery
invariants, and a runnable public-API example for the documented live-model
behavior. Run that example locally with a real model and its documented user
interaction before claiming that an example is verified, and record the result in
the pull request. A simulated external application or service boundary must be
named as such; never present it as a real third-party integration.

## Public API changes

When changing the public HTTP surface:

1. describe the Mango workflow and acceptance criteria in the Issue or pull
   request;
2. add or update raw HTTP golden tests for exact JSON and status behavior;
3. update the API docs and embedded `internal/httpapi/openapi.yaml`;
4. update `docs/capabilities.md` when a capability or user-visible limitation
   changes;
5. document data migration and rollback implications when persisted state
   changes;
6. update or remove obsolete wire tests when an intentional API change makes
   their old assumptions invalid.

Mango is pre-release. A public API may change in place when the change has a
clear product rationale and updates the implementation, OpenAPI, documentation,
and tests as one slice. Mango currently has no customers or supported releases,
so every API change targets `/v1` directly. Do not add `/v2`, version
negotiation, legacy shims, dual behavior, deprecation windows, or data readers
for earlier development snapshots. Update or remove old tests and fixtures
instead. Development databases may be recreated when the schema changes.

External documentation and public SDK behavior may be reused or adapted
deliberately. A change may retain a sound public route, resource shape, JSON
field, event name, or SDK-exposed type; do not rename it merely to appear
different. Once adopted, the result is Mango's wire contract and creates no
compatibility or synchronization obligation to the source. Cite material
influences and the adopted, changed, or rejected decisions in
`docs/provenance.md` or the relevant design document. Do not copy external
implementation code or non-public types. An existing third-party client test
is optional research evidence, not by itself a reason to preserve an API shape.

When the user problem and lifecycle match, prefer an established CMA design or
another widely used convention over inventing a Mango-only equivalent. Exact
field parity is not required: keep the fields Mango needs, reject hosted or
rollout-only details, and adapt semantics to self-hosting. Prefer standard HTTP,
simple general data shapes, and existing Mango primitives before introducing a
new header, wrapper, state, field, or abstraction.

## Sandbox backend changes

Before adding a substantial sandbox backend, describe the target use case,
trust boundary, host dependencies, network defaults, resource controls, session
persistence, and restart behavior in the pull request, a design document, or an
Issue.

Backend changes should preserve the provider contract and session-scoped
ownership described in the [sandbox backend guide](docs/sandboxes.md). Keep
external runtimes optional, keep default tests offline, add shared lifecycle
and tool-contract coverage, and label experimental integrations honestly.
Command execution alone is not evidence that a backend is production-ready or
safe for hostile multi-tenant workloads.

## Architecture expectations

- Keep wire DTOs in `internal/httpapi` and persistence/execution facts out of
  public responses.
- Preserve the event log as the authoritative public history.
- Do not perform model, sandbox, or other external work inside SQL transactions
  or application locks.
- Treat crash recovery and side-effect idempotency as part of a feature, not a
  later operational detail.
- Add interfaces at infrastructure/trust boundaries, not around every domain
  type.

## Pull requests

Keep each pull request focused. Include:

- a concise problem and solution statement;
- tests that fail without the change;
- API and migration impact;
- security considerations for tools, sandboxes, credentials, or external calls;
- documentation updates for user-visible behavior.

Use `gofmt` for Go code. Generated and dependency artifacts should not be
committed except for lockfiles required for reproducible builds.

Keep deployment assets within the support boundaries documented in
[`deployments/README.md`](deployments/README.md). The local Compose stack may
build the current checkout; future production bundles must consume versioned
release images and document their upgrade lifecycle.

By participating, you agree to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).
