---
title: Self-hosted boundary follow-up
description: Shutdown durability, explicit file transfer, and independent contract validation.
slug: /design/self-hosted-boundary-followup
---

# Self-hosted boundary follow-up

The operator needs to stop a worker without prematurely losing Memory writes,
and applications need a usable way to retrieve explicitly uploaded files after
the control-plane sandbox path was removed.

## Acceptance criteria

- The Docker reference allows the worker's result delivery, Memory teardown,
  and final Work Stop to finish before a hard kill. Ordinary cancellation keeps
  the lease renewable through teardown; lease loss still fences the worker.
- Replacing a reclaimed attempt must not spend the new claim's starting lease
  waiting for an expired worker to shut down. Validate the old container's
  identity, force-remove it, and start the new worker only after removal succeeds.
- An authenticated application can upload, list, retrieve, download, and delete
  an immutable File in its Workspace. Another Workspace cannot read its bytes.
  File metadata and queries do not retain unused Session scope or download
  eligibility fields. Upload/delete recovery and bounded text admission remain.
- OpenAPI, all three Mango SDKs, capability documentation, and independent HTTP
  and service tests describe that same File workflow.
- MCP documentation accurately states the bounded inline result and unsupported
  binary-content behavior, including the absence of a full-result file.
- No development or CI test uses the official Anthropic SDK as a Mango client.
  Keep independent HTTP and first-party SDK coverage and preserve persistence,
  recovery, and service assertions when replacing old research tests.
- Independent HTTP tests retain successful Session budget creation, increases,
  removal, and rejected additions; Mango SDK tests verify budget and usage
  amounts in Session and event responses.

## Design and non-goals

Files are immutable Workspace-owned objects in Mango's configured object store.
An application explicitly transfers sandbox deliverables and retains any
Session-to-File association itself. Reading an uploaded File does not make it
public and does not grant the scoped Work credential access to the Files API.
Mango retains the multipart upload and binary content endpoint, and removes
the now-unused `downloadable`, `scope`, and `scope_id` fields in place on `/v1`.
Development databases are rebuilt after the schema change; there is no migration
reader for an earlier unsupported checkout.

Automatic sandbox mounts, output publication, new providers, a general artifact
subsystem, private MCP connectivity, and additional language worker helpers are
outside this follow-up. External references and intentional differences are
recorded in `docs/provenance.md`.

## Verification ownership

The retired official-client suite duplicated HTTP validation and wire tests for
Agents, Sessions, events, budgets, resources, and pagination. Those independently
authored handler tests and raw JSON golden assertions remain. Mango client tests
own resource grouping, nullable updates, event variants, paging, and streams;
the Go, Python, and TypeScript conformance programs now also transfer binary
Files through the real HTTP handlers.

PostgreSQL/S3 tests retain File and Skill upload/delete recovery, Memory version
preconditions and redaction, Workspace isolation, and child Thread archival
with its durable orchestration intent. Worker tests own cancellation and lease
fencing; one real Docker shutdown test deliberately delays a Memory write beyond
the old 15-second kill deadline and verifies renewal, persistence, and cleanup.

## Follow-up deliveries completed

The original follow-up slices below have been delivered. Do not treat this
historical list as outstanding work; use the
[2026-09-30 assessment](../architecture/self-hosted-workers.md#assessment-on-2026-09-30)
and current capabilities when selecting another slice.

1. **Environment supervisor credentials — implemented.** The
   [credential design](environment-credentials.md) records the operator key
   lifecycle, Poll/Ack/Stats scope, transactional revocation, and independent
   in-flight Work leases.
2. **Worker execution healthcheck — implemented in PR #224.** The
   [healthcheck design](environment-healthcheck.md) and Work API describe the
   fixed, bounded sandbox probe with durable result and expiry. It proves that
   execution path, not arbitrary Session input preparation or provider health.
3. **Complete deliverable application — implemented in PR #218.** The
   [coding example](../examples/coding-agent.md) stages inputs, replaces its
   worker, independently verifies the result, and uploads/downloads Files.
   Runtime/recovery tests remain independent of the tutorial. Automatic mounts
   and output publication were explicit non-goals and remain operator-owned.

Additional providers and equivalent Python/TypeScript worker composition should
follow a demonstrated operator need. They are not prerequisites for validating
the current Docker/Go execution boundary.
