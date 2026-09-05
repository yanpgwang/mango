---
title: Memory
slug: /api/memory
---

# Memory

Memory Stores are PostgreSQL-backed, cross-Session collections of ordinary
UTF-8 files with immutable Version history. Memory routes use Mango's standard
bearer authentication and JSON content type rules.

The fourteen operations cover Store create/get/update/list/archive/delete,
Memory create/get/update/list/delete, and Version get/list/redact. The running
server's `/openapi.yaml` is the exact path and schema reference.

## Data model

- A Store holds at most 2,000 current Memories.
- Each Memory has a canonical absolute path and at most 102,400 bytes.
- Every create, update, and delete appends an immutable actor-attributed
  Version.
- Content mutations may use a SHA-256 optimistic precondition. A stale request
  that already describes the stored desired state is idempotent; a conflicting
  change returns `409 memory_precondition_failed_error`.
- Archiving a Store is one-way and makes it read-only. Delete is rejected while
  a Session remains attached.

## Agent access

Docker-backed cloud Sessions and the standalone self-hosted Docker worker mount
attached Stores beneath `/mnt/memory/<store-slug>/`. Store metadata and
instructions enter system context; file contents do not. Agents use the
ordinary `read`, `write`, `edit`, `glob`, `grep`, and `bash` tools rather than a
Memory-specific recall tool.

On the self-hosted path, the worker downloads all attachments from the frozen
Session before it constructs the toolset. `read_write` roots synchronize
through the existing Memory HTTP operations with SHA-256 preconditions;
`read_only` roots pull but never push. Concurrent remote changes win, local
deletes require a corroborating pass, a clean end performs a final sync, and
all exits receive a bounded push-only flush before trusted mount directories
are removed. The per-Work credential can access only attached Stores and every
mutation is transactionally fenced by its live lease.

The file tools reject writes into a read-only root. Bash has ordinary access
inside the Session container, so this is an agent-tool policy rather than a
kernel-enforced read-only mount. The Docker sandbox and its credential boundary
remain authoritative.

Automatic 30-day Version retention and non-Docker self-hosted launcher examples
are not implemented.
