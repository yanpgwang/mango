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
- Node.js 22 or newer for the documentation site and TypeScript SDK;
- Docker with Compose for service conformance and self-hosted worker tests.

Run the core checks:

```bash
make verify
```

### Test layers and ownership

Put a test at the lowest layer that can prove the requirement. Do not repeat a
pure package invariant in a system test, and do not use a cookbook application
as a runtime test harness.

| Layer | Repository location | Local command | Required CI job |
| --- | --- | --- | --- |
| Package behavior | Adjacent Go `_test.go` files; no network, Docker, or service dependencies | `make test` | Go |
| Concurrency safety | The same deterministic Go package tests under the race detector | `make test-race` | Go |
| HTTP and SDK contract | `internal/httpapi`, `scripts/sdk-contract`, and each `sdk/<language>` package | `make sdk-test && make sdk-conformance` | First-party SDKs |
| Stateful runtime integration | The package that owns the invariant, normally `internal/pg` or `internal/temporal`; use a descriptive `_integration_test.go` filename | `make test-service-core` | Core services |
| Self-hosted worker conformance | `internal/selfhosted` and the owning orchestration package for one composed vertical slice | `make test-self-hosted-docker` and, when orchestration is involved, `make test-service-core` | Self-hosted Docker worker and Core services |
| Distribution, docs, and security | Container, website, SDK packaging, and dependency entry points | `make image-smoke`, `make docs-check`, `make security` | Container, Documentation, Security checks |
| Real model and interactive journeys | An opt-in integration smoke or a standalone application in `examples/` | `scripts/with-dev-env make test-self-hosted-live` or the documented demo command | Never required in public CI |

For a cross-component feature, prefer one deterministic vertical test for the
successful user journey plus focused tests for retry, restart, cancellation,
idempotency, authorization, and cleanup invariants. Add a Python or TypeScript
test when that language's SDK behavior is the subject; keep Mango runtime and
durability assertions in Go so they run with the implementation and Go's race
detector.

`make lint` checks changes relative to `origin/main`, matching the incremental
CI rollout. Set `LINT_BASE` when your comparison branch differs.

Run the documentation checks:

```bash
make docs-check
```

The Fumadocs site reads `docs/` directly and exports a static GitHub Pages
artifact. Its build checks internal links, anchors, assets, search indexes, and
Markdown exports. SDK snippets use named regions from executable examples;
run `make sdk-test` and `make sdk-conformance` after changing those examples.
See [website/README.md](website/README.md) for authoring and local preview.

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

The combined target keeps the complete local verification entry point, while CI
runs its two layers in parallel:

```bash
make test-service-core
make test-self-hosted-docker
```

`test-service-core` owns tests that require PostgreSQL, Temporal, NATS, MinIO,
or a Docker-backed runtime. `test-self-hosted-docker` owns the reference
launcher, worker filesystem, and container-boundary contracts. Keep new
service-only packages in `SERVICE_CORE_PACKAGES`; keep launcher infrastructure
under the self-hosted target. Each layer has an explicit Go timeout below its CI
job timeout so test cleanup and failure diagnostics still have time to run.

On native Linux, use `make test-service SERVICE_TEST_EXEC='sudo -n -E --'`
with a trusted local checkout and passwordless sudo. This runs only the test
binaries as root, matching the Compose worker; Go compilation and caches keep
your user identity. Container-created bind-mount files retain their numeric
ownership, so an unprivileged runner cannot reliably remove nested outputs.
CI uses this mode for service tests while unit tests remain unprivileged.
Docker Desktop normally maps bind-mount ownership to the desktop user, so the
plain command above works there. Cleanup errors remain test failures.

Default tests must stay offline and deterministic. Service tests must use
isolated database schemas and clean up their workflows, File objects, worker
containers, and temporary workspaces. `make test-service` requires a reachable Docker daemon and sets
`MANGO_TEST_DOCKER=1`; required Docker checks must fail rather than skip when
the daemon becomes unavailable. The default-runtime test provisions the
binary's actual default image, verifies Python execution, reattaches its
workspace, and checks teardown independently of any cookbook application.
There is no host-process sandbox implementation or fallback. `make test` and
`make test-race` disable Docker checks; direct `go test` requires the explicit
flag above to enable them. Pure lifecycle/protocol tests use non-executing
doubles. Built-in tool tests run in real Docker containers. Test helper
containers and their temporary mounts are cleaned up even after assertion
failures.

A real model endpoint is a separate, explicitly enabled test tier
because it uses a credentialed network call and may incur cost:

```bash
make test-model-live
make test-self-hosted-live
make test-platform-live
```

`test-self-hosted-live` is the preferred smallest product smoke: one real
model-selected Bash call travels through authenticated HTTP, PostgreSQL,
Temporal, NATS, Environment Work, and a self-hosted Docker worker.
`test-platform-live` is an alias for it. Maintainers with a configured local
model endpoint should run this smoke before pushing substantial changes to the
model adapter, orchestration, Environment Work, or the self-hosted runner, and
record the result in the pull request. This is a best-effort maintainer check,
not a credential requirement for contributors or CI; when it cannot be run,
say so instead of presenting deterministic coverage as a live-model result.

The live targets require the `MANGO_MODEL_*` variables documented in
[model configuration guide](docs/guides/model-configuration.md). They are intentionally not run in public CI and must
never print or persist API keys.

The durable custom-tool gate scenario can be selected with
`make test-hitl-gate`; its credentialed user journey runs with
`scripts/with-dev-env make demo-hitl-gate` against the public HTTP API. Keep
its documented example aligned with the complete-barrier, partial-result,
duplicate-result, worker-replacement, and interactive live-model assertions.

The specialist-team user journey runs with
`scripts/with-dev-env make demo-multi-agent-team`. Keep it aligned with the
ordinary-child, Advisor, real-usage, completion-barrier, and persistent-follow-up
contracts covered by the multi-agent service and runtime tests.

Cookbook-derived examples are standalone public-API/SDK applications. Keep their
scripts and input data in `examples/`; system tests own their fixtures and
setup and must not execute cookbook examples or import their data. Do not add a
dedicated system-test or CI harness just to run a tutorial. Runtime and recovery
invariants remain covered by independent system tests. Run an example against
a running Mango deployment with a real model and its documented user interaction
before claiming it is verified, and record the result in the pull request.
A simulated external application or service boundary must be named as such;
never present it as a real third-party integration.

## Public API changes

First-party SDKs are generated from the checked-in OpenAPI source. After an API
or schema change, run `make sdk-generate` and `make sdk-check`. Install SDK
development dependencies with `make sdk-install`, then run `make sdk-test`
and `make sdk-conformance`. The latter executes all language clients against
the real HTTP handlers with test-only storage/model fakes; it does not replace
the service or live-model verification tiers. Keep SDK source packages free of
server runtime dependencies and do not publish packages as part of development
without an explicit release request.

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

## Self-hosted worker changes

Before adding a substantial self-hosted launcher or worker capability, describe
the target use case, trust boundary, host dependencies, network defaults,
resource controls, session persistence, and restart behavior in the pull
request, a design document, or an Issue.

Launcher changes should preserve the provider-neutral Environment Work protocol
and operator-owned lifecycle described in the [self-hosted sandbox guide](docs/sandboxes.md).
Keep external runtimes outside the control plane, keep default tests offline,
add shared lifecycle and tool-contract coverage, and label experimental launcher
examples honestly. Command execution alone is not evidence that a launcher is
production-ready or safe for hostile multi-tenant workloads.

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

Use `gofmt` for Go code. Generated build and dependency artifacts should not be
committed except for lockfiles required for reproducible builds. First-party
SDK bindings and the SDK OpenAPI snapshot are a narrow exception: they are
distributed source, generated offline from `internal/httpapi/openapi.yaml`.
Keep their generators in the repository and run the SDK drift checks whenever
the contract or a generator changes. Never edit generated bindings by hand.

Keep deployment assets within the support boundaries documented in
[`deployments/README.md`](deployments/README.md). The local Compose stack may
build the current checkout; future production bundles must consume versioned
release images and document their upgrade lifecycle.

By participating, you agree to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).
