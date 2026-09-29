---
title: API readiness design
description: Bound database admission probes without coupling liveness to execution dependencies.
---

# API readiness

## Problem and acceptance criteria

An operator must be able to remove an API process from traffic when its database
cannot admit durable requests, without restarting a live process because a
recoverable dependency is unavailable.

- Public `GET /healthz` reports process liveness with an empty 200 response and
  performs no dependency I/O.
- Public `GET /readyz` checks the API's PostgreSQL pool with a two-second request
  deadline. It returns an empty 200 when a connection can execute a query in a
  writable transaction, and a sanitized standard Mango error with status 503
  when unavailable, read-only, timed out, or unconfigured. Recovery is visible on
  the next probe. Probe responses must not be cached by intermediaries.
- Temporal, NATS, model providers, object storage, and Environment workers do not
  gate API readiness. PostgreSQL admission and the durable outbox preserve work
  through asynchronous execution outages.
- Raw HTTP, PostgreSQL failure/recovery, and first-party SDK checks establish the
  contract. A successful probe is a point-in-time connection and transaction-mode
  check, not proof of disk capacity, every table permission, or future writes.

## Implementation and validation

Inject one context-aware readiness check into the HTTP server. The PostgreSQL
store executes a read-only observation of `transaction_read_only` through the
same pool used by API requests. Bound it in the HTTP handler; never expose the
underlying database error. Keep successful response bodies empty and use the
existing error envelope on failure. Do not add schema tables or probe writes.

Write HTTP regression tests first, then wire the store and production server.
Test writable/read-only recovery and exhausted-pool timeout/recovery against real
PostgreSQL, independently of shared service restarts. Update OpenAPI, generated
SDK contracts, operator documentation, capabilities, and provenance together.

## Scope and history

This extracts the useful readiness idea from closed, unmerged PR #57 and commit
`e39b182ff19d2374fc09ae509d931be871bb5c82`. Its multi-dependency hard gate is
rejected: NATS is an optimization and Temporal execution is asynchronous. Worker
execution readiness belongs to a later bounded Environment healthcheck Work PR.
This slice does not add metrics, logging infrastructure, an aggregate dependency
status API, or change API startup dependency requirements.

The next independent slice restricts supervisor credentials to one Environment.
Bounded worker healthcheck Work follows that credential boundary. Session
inspection and SDK release preparation remain subsequent slices.
