# Security policy

`mango` is pre-release software and has not received a security
audit. It should not be exposed as a production multi-tenant service without an
independent review.

## Supported versions

Until the first stable release, security fixes target the latest commit on
`main`. Older commits and development database schemas are not supported.

## Reporting a vulnerability

Please use the repository's private GitHub Security Advisory reporting flow:

`https://github.com/yanpgwang/mango/security/advisories/new`

Include affected versions, reproduction steps, impact, and any suggested
mitigation. Do not include credentials or sensitive production data. Please do
not open a public issue for an unpatched vulnerability.

If private reporting is unavailable, open a public issue requesting a private
maintainer contact without disclosing vulnerability details.

## Current security boundaries

- Docker is the default runtime sandbox; host-process execution is not
  selectable. Containers share the host kernel and the provider has not been
  audited for hostile multi-tenant workloads. Direct provider calls default to
  no network, while cloud Environments request bridge networking unless a
  supported network policy says otherwise.
- The local Compose worker is trusted with root access and the host Docker
  socket. The API and Session containers do not receive that socket; Session
  containers do not inherit worker credentials. Docker access still grants
  substantial host authority and is not a hostile multi-tenant guarantee.
- Every protected API request is authenticated by an opaque API key and scoped
  to one Workspace. Top-level resources, child resources, scheduled work, and
  object-store keys are isolated by that Workspace. Health, readiness, and the
  embedded OpenAPI document remain public.
- All keys for one Workspace have identical access to that Workspace. Mango
  does not model end users, roles, per-resource grants, or user-level audit
  identity; a SaaS or enterprise control plane must own those concerns and
  issue or revoke Workspace keys.
- Protected HTTP routes require a Workspace key in `Authorization: Bearer`.
  Requests with non-empty bodies must use the documented JSON or multipart
  content type. Provider version and beta headers are not part of Mango's API.
- PostgreSQL journals tool attempts, but an external side effect can still be
  ambiguous if execution succeeds and its durable result is lost. Exactly-once
  behavior requires idempotency from the external system.
- Model credentials are read from environment variables. Operators are
  responsible for secret storage, rotation, logging policy, and endpoint trust.

See the [architecture](docs/architecture.md),
[product direction](docs/product.md), and
[capabilities and limits](docs/capabilities.md) for current boundaries and
planned hardening priorities.
