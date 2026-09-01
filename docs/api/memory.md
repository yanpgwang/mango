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

OpenSandbox-backed cloud Sessions mount attached Stores beneath
`/mnt/memory/<store-slug>/`. Store metadata and instructions enter system
context; file contents do not. Agents use the ordinary `read`, `write`, `edit`,
`glob`, `grep`, and `bash` tools rather than a Memory-specific recall tool.

Read/write mounts synchronize changes back to PostgreSQL after sandbox tools
and perform a final writeback before sandbox deletion. Each Store uses one
OpenSandbox-managed volume mounted publicly with the requested access and
privately for trusted Mango refresh/writeback; UID 1000 cannot traverse the
private control root.

Automatic 30-day Version retention is not implemented.
