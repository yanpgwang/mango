---
title: Product direction
description: The principles and release policy that guide Mango’s development.
slug: /product
---

# Product direction

Mango is an open-source, self-hosted alternative to Claude Managed Agents.
Applications define Agents and submit work through Sessions; Mango manages the
agent loop, conversation state, tool coordination, and recovery. Operators run
the control plane, state, and workers on infrastructure they control.

Mango began with resource and workflow ideas documented by Claude Managed
Agents. Its routes, resource models, JSON shapes, events, and public SDK types
remain useful design starting points. Mango may preserve a sound design when it
fits instead of inventing or renaming an equivalent surface. Drop-in use with
an Anthropic service or SDK is not a product goal: once adopted, the resulting
contract belongs to Mango and may be simplified, extended, or replaced when
the self-hosted runtime needs something different.

## What Mango optimizes for

- **Durable work.** Accepted input, events, tool calls, waits, retries, and
  cancellation survive process restarts.
- **Operator control.** State, credentials, sandboxes, schedules, and model
  traffic remain within infrastructure and providers selected by the operator.
- **Recoverable execution.** Crash boundaries, idempotency, reconciliation,
  and upgrade behavior are part of a feature rather than follow-up work.
- **Composable infrastructure.** Model, sandbox, object-storage, and messaging
  integrations sit behind explicit boundaries and can evolve independently.
- **A coherent native API.** Mango's own documentation and OpenAPI definition
  describe the supported contract. New surface area must improve a real user
  or operator workflow.

## Relationship to external contracts

Public specifications and SDKs may provide terminology, routes, resource
models, JSON schemas, event types, public client types, workflow examples, and
edge cases. Mango deliberately reuses or adapts them when that reduces
unnecessary invention and serves its users. Similarity is acceptable; it does
not make the source an authority over Mango or create a synchronization
obligation. In particular:

- users are not required to use an Anthropic SDK;
- Mango does not promise drop-in use with a hosted agent platform;
- an external SDK release does not automatically create Mango work;
- public contract comparisons are research evidence, not obligations; Mango's
  independent HTTP, SDK, and runtime tests define its executable checks;
- vendor-specific previews are considered only when they solve an independently
  selected Mango problem.

Mango currently has no customers and no supported stable release. `/v1` is the
single development namespace, not a Mango 1.0 or stable API declaration.
Routes, fields, schemas, and behavior change directly on
`/v1` when that produces a clearer product. Earlier commits, SDK behavior,
fixtures, tests, and development databases create no compatibility obligation.
Do not add `/v2`, legacy routes, dual behavior, deprecation windows, translation
layers, or data migrations solely to preserve them. This policy changes only
after an explicit maintainer decision establishes a supported release or a real
customer migration requirement.

## Choosing work

A substantial change should answer three questions before implementation:

1. Which concrete Mango user or operator workflow improves?
2. Which durability, security, recovery, and operational boundaries apply?
3. What is the smallest independently testable slice?

Priority goes to deployment, upgrades, observability, reliable Session
execution, safe tool and sandbox operation, Files and deliverables, persistent
Memory, and a clear native developer experience. Breadth, endpoint counts, and
parity with another product are not measures of progress.

## Current transition

The implementation already contains a broad API, but existing surface area is
not automatically permanent. During the transition:

- [Capabilities and limits](capabilities.md) is the honest inventory of what
  Mango currently does;
- [API reference](api/overview.md) and the served OpenAPI document define the
  current HTTP surface;
- current source, migrations, documentation, and executable tests are the
  starting point for implementation decisions;
- [GitHub pull requests](https://github.com/yanpgwang/mango/pulls), design
  documents, and optional [Issues](https://github.com/yanpgwang/mango/issues)
  may carry active work and rationale;
- differences from external contracts are design inputs, not backlog items,
  unless a Mango change gives them a user or operator rationale and acceptance
  criteria.
