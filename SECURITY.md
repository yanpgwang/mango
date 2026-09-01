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

- OpenSandbox is Mango's only sandbox control plane; host-process and direct
  Docker execution are not selectable. Local and CI use OpenSandbox's Docker
  runtime, whose Session containers share the host kernel and are not a
  hostile multi-tenant boundary. The Kubernetes/Kata profile remains a
  qualification candidate, not a security certification. Direct provider
  calls default to no network, while cloud Environments request unrestricted
  networking unless their policy is limited.
- In local Compose only the trusted OpenSandbox service mounts the host Docker
  socket. The Mango API and worker retain the image's non-root user; the API
  receives neither the socket nor OpenSandbox credentials, while the worker
  receives only the private OpenSandbox endpoint and key needed for execution.
  Session containers do not inherit worker/model/object-store credentials.
  Compromise of the OpenSandbox service still grants substantial daemon
  authority.
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
