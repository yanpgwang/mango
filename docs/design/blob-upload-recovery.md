---
title: Upload recovery across API processes
description: Preserve live Files and Skills uploads while collecting crash leftovers.
---

# Upload recovery across API processes

An API process starting against shared PostgreSQL and object storage previously
deleted every non-ready File and Skill Version. Those rows may belong to another
process's live upload. Independent two-pool PostgreSQL/SeaweedFS reproductions
paused uploads at object I/O: Files lost their metadata, and Skills lost an
already written archive.

## Acceptance criteria

- Starting or running a second API preserves the first API's active upload.
- Cleanup must recheck state atomically; a stale list cannot delete a File or
  Version that has since become ready.
- Completed bytes survive a lost completion response.
- Ended/expired uploads and unfinished deletes are eventually cleaned, including
  a restart before a crashed upload's lease expires and temporary storage outages.
- An expired writer cannot renew or publish. A late completed object write is
  cleaned without affecting a new upload with the same time-based Skill Version.
- Workspace authorization and generated MCP File pins retain their existing
  contract. Public API types and first-party SDK operation grouping stay intact.

## Ownership and cleanup

Ordinary uploads commit a one-minute lease using PostgreSQL wall time. A request
renews it every 20 seconds while object I/O and completion are in progress.
Renewal failure cancels the request's I/O context; SQL completion independently
checks that the lease is current. Renewal and completion acquire the row lock
first, then check database wall time in a second statement within the same short
transaction; waiting for a lock cannot authorize an already expired writer. A request ending releases only its pending
intent. A crashed request stops renewing and becomes eligible after expiry.

Reconciliation lists candidates, then atomically claims an ended or expired
upload by changing its state to `deleting`. A completed or still-owned candidate
is skipped. No external I/O runs inside a database transaction or application
lock. API startup performs a scan; a cancellable background loop scans every
20 seconds and retries storage failures. Metadata and guard scans each have a five-second budget, so a slow metadata
deletion cannot consume the guard queue's entire periodic opportunity. Once the
object store is connected, startup cleanup failures keep the resource APIs
enabled and register the periodic retry. Initial storage connection failures
retain the existing disabled-feature behavior.

Every ordinary File upload has a new random File ID. Skill Versions use time
identifiers that can be reused after an abandoned Version is removed. Each Skill
upload therefore has its own random archive key. Renewal, completion, release,
and incomplete-row removal verify that key. A late old writer can delete its own
bytes but cannot publish into, release, or remove the replacement upload.

Unknown database completion errors retain bytes because publication may already
have committed. Definite ownership failure permits cleanup of the old upload's
private object. Ready resources never become cleanup candidates. Object deletion
is idempotent, and failed deletion retains the `deleting` intent for a later scan.

Claiming an abandoned upload atomically retains an independent object cleanup
guard using the claimed row's Workspace. This happens before removing metadata
or deleting bytes, so even a late successful write followed immediately by a
writer crash remains recoverable. Repeating a claim preserves any recorded
writer acknowledgement. Request release records pending-upload termination and
can acknowledge an existing guard without recreating cleanup for a ready File
or Version.

Only a successful object write or a positively identified pre-write failure
confirms that the writer finished. S3 spool failures are identified separately;
a network error or cancellation during `PutObject` may leave the remote outcome
unknown. Guards for unknown writers remain and continue deleting late bytes.
A guard is removed only after a confirmed finish and successful deletion. This
depends on the provider treating a successful write response as a completed
write. `PutObject` makes one remote attempt: SDK retries are disabled for writes
because a later successful retry cannot settle an earlier unknown attempt. Read
and idempotent delete retries retain their ordinary SDK behavior. Mango does not
infer remote completion from a timeout.

Revisions use a database sequence and are never reused after a guard is removed
and recreated. A stale deletion acknowledgement cannot clear a newer guard.
The queue takes at most 100 records, oldest checked first, and records each
attempt before storage I/O. Long-lived guards therefore rotate behind unvisited
keys instead of monopolizing every bounded scan. Unconfirmed crash guards are
small persistent metadata; automatic time-based deletion would reopen the late
write gap. Operators must retain them while an old writer or remote request
might still commit. General inventory/compaction is outside this change.

Cancellation closes a closable upload source while spooling. The HTTP multipart
adapter first expires the actual connection's read deadline, then closes the
outer body. HTTP/1 Body.Close can wait on a blocked read; closing only the
multipart part drains more input and can block too. Arbitrary embedded
non-closable readers are checked between reads; callers must supply an
interruptible closer if a read can stall indefinitely.

Generated MCP output uses its completed durable receipt and Session pin instead
of an ordinary request lease. It remains resumable while its Session exists;
Session deletion makes an abandoned pending result eligible for cleanup. The
existing generated-output workflow may retry a File key after an unknown remote
write; tracking prior attempts through ready publication and later deletion is
a separate follow-up. This slice establishes ordinary File/Skill upload ownership
and does not claim that broader MCP retry boundary.

## Scope and evidence

The development baseline adds nullable internal expiry columns and independent
object cleanup guards; sqlc is regenerated. Use fresh development databases for incompatible checkout changes.
There are no public lease fields or hosted dependencies. This fixes a specific
shared-storage recovery boundary; it does not establish a supported production
or Kubernetes distribution, general quotas, backup/restore, or object-store
behavior beyond the configured provider's request and deletion semantics.

Independent PostgreSQL and SeaweedFS tests use two pools in one isolated schema.
They cover live uploads on both sides of object publication, expiry, stale
writers and reused Versions, ready-byte preservation after lost responses, and
crash cleanup, late-write cleanup after a deletion outage, writer crashes after
late publication, non-reused acknowledgements, and fair bounded guard scans. Package tests
cover renewal cancellation, pipe and real HTTP/1 source cancellation, and periodic retry/
shutdown. Existing HTTP/SDK, pin, authorization, persistence, workflow, and
Docker suites remain separate checks.
