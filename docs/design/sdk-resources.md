---
title: SDK resource design
---

# SDK resource design

The SDK should let an application configure an Agent team, start a Session,
observe its Threads and Events, and run a self-hosted worker without translating
HTTP operation IDs into application structure. The previous clients exposed a
flat method inventory and required Python/TypeScript callers to wrap request
fields in `body`. Go's common configuration unions also required excessive
construction boilerplate.

## Acceptance criteria

- All three clients expose every current operation through resource services:
  Agents and Versions; Environments and Work; Sessions, Threads, Events and
  Resources; Files; Skills and Versions; Memory Stores; Vaults; Deployments;
  Webhooks; and System diagnostics. No parallel legacy client surface remains.
- Python accepts typed keyword arguments; TypeScript accepts request fields
  directly. Path identifiers are positional in HTTP hierarchy order across all
  languages. Go uses typed request structs and concise constructors for common
  Agent, roster and message unions.
- Resource/method mappings are explicit in OpenAPI and checked by the exporter.
  Generated clients preserve omission, null, false, empty values, multipart
  bytes, errors, cursor filters and live SSE ownership.
- Existing worker helpers, callers, runnable quickstarts and documentation use
  the new surface. An independently authored multiagent SDK contract journey
  checks request and response mapping, separate from runtime durability tests.
- SDK generation checks, language tests/typechecks, HTTP conformance, relevant
  Go caller tests and documentation checks pass.

## Boundaries and references

This changes the pre-release SDK directly. HTTP routes, lifecycle semantics,
storage and model execution remain the current Mango contract. Automatic write
retries, a declarative apply CLI, package publishing, and unrelated runtime
changes are outside this SDK design slice. Python retains explicit TypedDict
response values; attribute access is not required to solve the workflow.

The paired public CMA Agent/multiagent APIs and official Python v1.4.0,
TypeScript sdk-v0.124.0 and Go v1.71.0 SDK sources were reviewed on 2026-09-07.
Mango adopts resource grouping and direct language-idiomatic request parameters.
It keeps its own transport, types, authentication and worker lifecycle, with no
Anthropic dependency, beta namespace, hosted credentials or implementation copy.
The common parent-first positional path order makes nested Mango resources
consistent across the three languages. Generated service types share one client
transport; they neither own separate connections nor cache resource state.

Validation must independently check concrete HTTP paths and payloads; matching
the generated manifest alone is only an inventory check. The persisted event log
continues to resolve ambiguous sends. SSE remains live-only, without automatic
reconnection or pretending that preview deltas are durable events.

## Validation record

Validated on 2026-09-07 against main `5ee63a8` (self-hosted Environment default):

- `make sdk-test`: generation drift checks, Go race tests and vet, 125 Python
  tests plus mypy/ruff, and 22 TypeScript tests plus client/example typechecks.
- `make sdk-conformance`: all three clients and the documentation quickstarts
  passed against Mango HTTP handlers with test-only storage/model implementations.
- OpenAPI contract tests and the SDK contract, self-hosted and Temporal package
  tests passed. The full root package tree compiled, and `make lint` reported no
  new issues.
- `make docs-check` passed the site tests/build and static export checks.
- Python wheel and sdist, and the npm tarball, were inspected and installed in
  fresh environments outside the checkout. Public imports and authenticated
  requests passed; Python typing markers and TypeScript declarations were checked.
- `scripts/with-dev-env make test-self-hosted-live` passed with an actual model
  request and Docker Bash execution through the Environment Work lifecycle.
- Each standalone Python, TypeScript and Go multiagent application completed an
  initial turn and a reviewer follow-up against a temporary Mango deployment with
  an isolated database and Temporal namespace and the configured real model.
  Each finished with the primary and two specialist Threads idle and cleaned up
  its own resources. The optional Advisor was covered by contract tests rather
  than enabled in these three manual runs.

Independent subagent review found and fixed unresolved Python forward references
when inspecting resource methods with `typing.get_type_hints`; a regression test
covers synchronous and asynchronous resource signatures. Caller review also
migrated the coding-agent application and added it to mypy's checked files.
That application subsequently completed a real-model repair, downloaded its
result, verified the original checks in Docker, and cleaned up its resources.

The packages remain unpublished development candidates.
