---
title: Workspace tenancy
---

# Workspace tenancy

Mango uses a Workspace as its only tenant and authorization boundary. This is
deliberately narrower than an application user or enterprise RBAC model: it is
enough for the OSS runtime to demonstrate a managed agent serving multiple
tenants without making Mango responsible for a SaaS product's identities.

## Contract

- Every protected HTTP request presents an opaque bearer credential.
- An operator API key resolves to exactly one Workspace before a handler runs.
- All API keys for a Workspace have equal access to all resources in it.
- A self-hosted Work claim additionally receives a per-Session token limited to
  its Work lease operations, Session read/stream and tool-result APIs, and pinned
  immutable inputs. The token becomes usable after Ack and is continuously
  fenced by the Work lease.
- A resource from another Workspace behaves as not found; lists are filtered.
- Health, readiness, and the embedded OpenAPI document are public.
- Workspace IDs are internal and never added to public request or response bodies.

There are no users, roles, ownership inheritance, general per-resource grants,
or permission-policy engine. The Work token is a fixed internal execution
capability, not an application RBAC system. A SaaS layer may authenticate many
users, decide what they may do, and then call Mango with the key for their
Workspace. If enterprise requirements later need finer policy, that layer can
introduce OpenFGA or a similar system without changing Mango resource semantics.

## Persistence and execution

`agents`, `environments`, `sessions`, `files`, `skills`, `memory_stores`,
`vaults`, and `deployments` carry `workspace_id`. Child tables inherit ownership
through their parent. Repository reads and mutations filter the root; session
creation also locks every referenced Agent, Environment, File, Skill, Memory
Store, and Vault in the same Workspace.

Workers use a separate system persistence view because Temporal activities and
reconcilers start from authoritative globally unique root IDs. Whenever an
operation crosses an asynchronous boundary that later creates or deletes
tenant data—scheduled Deployments, sandbox resource materialization, or
deletion reconciliation—the Workspace is recovered from the root row and put
back into the execution context. System-store tenant writes and dependency
reads fail closed when that scope is missing; they never fall back to
`wrkspc_default`. Single-tenant embedders use an explicitly default-Workspace
store instead.

S3-compatible objects are stored below `<workspace_id>/...`. Existing database
rows are migrated to `wrkspc_default`; their stored object keys remain readable.

## Keys and operations

Only key digests are stored. The operator CLI is intentionally local and talks
directly to PostgreSQL; Mango does not expose a Workspace administration HTTP
surface.

```sh
mango workspace list
mango workspace create -name acme
mango api-key create -workspace wrkspc_... -label production
mango api-key list -workspace wrkspc_...
mango api-key revoke -id key_...
```

`MANGO_API_KEY` binds an operator-supplied bootstrap key to
`wrkspc_default`, which preserves a simple single-Workspace deployment and a
smooth upgrade for pre-tenancy data.

## Non-goals

Workspace data isolation does not make a sandbox safe for hostile multi-tenant
code. Docker shares the host kernel, and its worker is trusted with daemon
access. Operators must select and harden an execution provider appropriate for
their threat model.
