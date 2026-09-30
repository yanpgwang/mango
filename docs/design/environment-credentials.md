---
title: Environment supervisor credentials
description: Scoped polling credentials, revocation ordering, and operator lifecycle.
---

# Environment supervisor credentials

## Problem and acceptance criteria

An operator currently gives every self-hosted polling supervisor a Workspace-wide
API key. A compromised supervisor consequently reaches unrelated Sessions, Files,
and Vaults. Give the supervisor a standing credential restricted to one
self-hosted Environment while preserving the existing per-Work execution lease.

- Operators issue, list, revoke, and rotate Environment keys through the existing
  database-backed `mango api-key` CLI. Creation returns the random secret once;
  only its SHA-256 digest is persisted. Listings identify the Environment scope.
- A key permits only `GET /v1/environments/{id}/work/poll`,
  `POST /v1/environments/{id}/work/{work_id}/ack`, and
  `GET /v1/environments/{id}/work/stats` for its Environment. Stats supports local
  supervisor diagnostics. No Session, File, Vault, Environment management, Work
  metadata, Work listing, or per-Work execution access is granted.
- Wrong-resource requests return 403; invalid or revoked credentials return 401.
  A revocation discovered after initial authentication also returns 401.
- Poll and Ack revalidate and lock the key in their own transaction. Revoke waits
  for claims/Acks already holding that lock; once revoke commits, no subsequent
  claim or Ack can commit using the revoked key, including authenticated requests
  and long polls already in progress. No external work runs inside that lock.
- Revoking a supervisor key does not cancel already-Acked Work. Its existing
  per-Work token can heartbeat, publish tool results, and stop until the usual
  lease expiry, reclaim, or termination. Unacknowledged claims expire under the
  existing reclaim age. A replacement Environment key can Ack pending Work in
  the same Environment; keys identify supervisors, not ownership of a claim.
- The Docker supervisor accepts `MANGO_ENVIRONMENT_KEY`. Sandbox children still
  receive only their per-Work secret. First-party SDKs use their existing bearer
  configuration and retain the Environments.Work resource mapping.

## Design and alternatives

Add nullable `environment_id` to `api_keys`, with a composite foreign key to the
Environment and Workspace. A null scope continues to mean a Workspace key.
Separate authentication resolution prevents an Environment key becoming a
Workspace principal. Transactions lock Environment, then credential, then
poller/Work rows to avoid a cycle with cascading Environment deletion. An Environment scope in request context carries the key ID
and digest for transactional fencing. Use a shared row lock for claims/Acks;
revocation takes the normal UPDATE lock. The middleware route allowlist is
explicit and defaults to deny.

A separate credential table would duplicate the same operator lifecycle. General
scopes/RBAC and separate HTTP credential-management resources add policy without
serving this slice. Keep creation in the trusted operator CLI, using
`api-key create -workspace ID -environment ID -label LABEL`; existing list and
revoke operations cover both key kinds. Rotate by creating and deploying a
replacement, then revoking the old key. Workspace keys remain useful for trusted
application administration, but worker CLI configuration names its narrower
credential explicitly and does not fall back to MANGO_API_KEY.

## Durability, migration, and non-goals

The schema includes the nullable scope and its referential integrity. Workspace
keys have no Environment scope; deleting an Environment removes its scoped keys.
The original migration 40 and its downgrade path were consolidated into the
[development schema baseline](../deployment.md#development-database-baseline).
Mango is pre-release; development databases from the historical chain must be
rebuilt, and no old-checkout data compatibility layer is provided.

No IAM/RBAC framework, hosted authentication, new API namespace, automatic expiry,
healthcheck Work, worker heartbeat command, credential sharing with sandboxes, or
change to Session lease ownership is included.

## Paired clean-room references (reviewed 2026-09-29)

Current official [self-hosted sandbox guide](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes),
[security model](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-security),
and [Environment Work API](https://platform.claude.com/docs/en/api/beta/environments/work)
were compared with Mango's middleware, migrations, Work repositories, SDK poller,
worker, and Docker launcher. CMA separates standing Environment keys from
per-session secrets and exposes Poll/Ack under Environments.Work. Mango retains
that useful credential separation and resource mapping. CMA's Console-only key
issuance and hosted API/beta authentication do not serve Mango's self-hosted
operator model; Mango uses its existing local CLI and standard Bearer header.
The cookbook Docker example forwards its standing Environment key into each
container. Mango rejects that broader sandbox authority: it requires the
per-Work secret and never falls back to a standing key for Session execution. The three-method permission set is Mango's own boundary.

Local official source reviewed as design evidence only:

- Go v1.76.0, ad865dfa3d1a8d2f4a7ad0d072011e811e9957a9:
  `betaenvironmentwork.go`, `lib/environments/poller.go`.
- Python v1.9.0, a7285e919ab79998d9380b3b57f6315b7860b8d8:
  `src/anthropic/lib/environments/_worker.py`.
- TypeScript sdk-v0.129.0, bf2058689f845dfb10e59bd9ebeb5cb4e9318a9d:
  `src/lib/environments/worker.ts`.
- Cookbook d7265d6ae994ccd8429db0594b000073b2f9ad43:
  `managed_agents/self_hosted_sandboxes/README.md` and Docker workflow.

No external SDK implementation is copied, executed as a Mango client, or added
as a dependency. The public workflow is beta despite the tagged SDK releases.

## Implementation plan

1. Add failing raw HTTP scope/status tests and PostgreSQL issue/revoke tests.
   Extend request scope, key persistence/authentication, and middleware. Verify
   correct Workspace/Environment binding and secret non-disclosure.
2. Add revoke-versus-Poll/Ack tests with database locks, then transactionally fence
   both operations. Verify replacement keys and already-Acked Work tokens.
3. Update operator CLI and Docker supervisor configuration with CLI tests and a
   first-party SDK HTTP journey. Preserve per-Work sandbox isolation tests.
4. Update OpenAPI, generated snapshot, operator/API/capability/provenance docs.
   Run focused tests, make verify, SDK and documentation checks, required service
   suites, and available self-hosted live smoke; review and create a draft PR.
