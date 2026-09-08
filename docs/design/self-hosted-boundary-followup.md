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

## Suggested next delivery slices

These are proposed work items, not current capabilities or an automatic CMA
parity backlog. Select and design one slice before implementation.

1. **Limit supervisor credentials to one Environment.** Operators should not
   need a Workspace application key on a polling host. Define issuance,
   revocation, and rotation for an Environment-scoped poll/Ack credential.
   Acceptance: it cannot read Files, Vaults, or unrelated Sessions or queues;
   revocation fences new claims; already-issued Work credentials retain their
   explicit lease lifecycle. Reuse existing key and Work primitives where they
   fit. Compare both CMA's public API and SDK before selecting the wire design.
2. **Make worker readiness observable.** An empty queue does not establish that
   a worker can launch, reach Mango, prepare inputs, or renew a lease. Design a
   bounded health-check Work flow with operator-visible results and expiry.
   Acceptance: distinguish unavailable workers, launch failure, and successful
   execution; retries and stale results do not misreport readiness. Keep it
   separate from application Sessions and an unrelated monitoring platform.
3. **Restore a complete deliverable application.** Build one standalone coding
   workflow that stages operator-owned inputs, executes a Session, checks the
   output, uploads selected Files, and downloads them with the application key.
   Acceptance: document credential ownership and interruption/retry behavior,
   run it against a real local deployment and model, and keep runtime/recovery
   tests independent of the example. Its evidence should determine whether a
   subsequent artifact association or large-MCP-result transfer is needed.

Additional providers and equivalent Python/TypeScript worker composition should
follow a demonstrated operator need. They are not prerequisites for validating
the current Docker/Go execution boundary.
