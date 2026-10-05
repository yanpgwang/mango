---
title: Kubernetes alpha conformance and recovery
description: Prove the control-plane chart with external state and sandbox execution.
---

# Kubernetes alpha conformance and recovery

Delivery 3 of the approved [alpha scope](kubernetes-alpha.md) proves installation
and the current same-release lifecycle. Operators need evidence that accepted
input, pending actions and stored bytes survive control-plane replacement and a
consistent restore before relying on the chart candidate.

## Topology and boundaries

An opt-in Go system test owns a uniquely named kind cluster, explicit kubeconfig,
and two uniquely named Docker Compose fixture projects. The fixture projects
provide PostgreSQL 17.5, Temporal 1.29.7, NATS 2.11.17, SeaweedFS 4.48 and an
explicitly simulated Messages endpoint. They live outside the chart. Connecting
the kind node to each fixture network permits Pod access through inspected
container IPs. No existing local Compose stack or Kubernetes context is changed.

The test installs the real Helm chart with existing Secrets, a non-default
model endpoint, the original fixture Vault keyring and versioned test images.
Test-only NodePort Services expose the two installations through loopback kind
port mappings; the chart continues to provide ClusterIP. A native operator
supervisor runs `mango-worker docker` with an Environment-scoped key, and its
item containers reach the API on the kind network. Sandbox execution never
moves into Kubernetes or a host-process substitute.

The Messages fixture is stateless: it derives the next response from the
supplied transcript, validates tool results, and supports JSON and SSE. Its
closed set of test commands is only test data, not a production model option.
The test never uses hosted agent or real provider credentials.

## Observable acceptance

1. A fresh chart install completes the migration hook and serves authenticated
   HTTP. Unauthenticated requests fail; `/healthz` and `/readyz` work.
2. Upload/download a File and canonical Skill bundle, attach the Skill and a
   read-write Memory Store to a Session, and execute a real Bash call through
   the external worker. Verify the Skill was prepared and Memory changes are
   visible over HTTP after worker completion.
3. Replace API and orchestration while a tool confirmation is pending. Preserve
   the exact public action ID and event history, allow it once, reject a duplicate,
   and complete without duplicating the tool side effect. Then reactivate and
   read the sandbox workspace and Memory again.
4. Leave a custom-tool action pending. Quiesce the external supervisor, API and
   orchestration, then stop Temporal. Dump the Mango, Temporal and visibility
   databases while no writer is running; copy all bucket objects and retain the
   original keyring. Restore into independent PostgreSQL and object storage,
   start the same Temporal version, and install the same chart with migration
   explicitly disabled. Original state stores remain quiesced.
5. Verify File bytes/checksum, canonical Skill bytes/checksum, Memory content
   and Version history and original pending event IDs from restored HTTP. Submit
   the outstanding custom result and observe a resumed model turn from restored
   Workflow history. Read-only Temporal queries verify the original Run ID and
   complete history prefix before submission, then prove that same execution
   advances; PostgreSQL reconstruction cannot satisfy this witness. GET of a
   restored fixture credential must succeed with its
   original keyring: the existing Vault service decrypts and verifies the stored
   envelope before returning public metadata. No secret is returned by HTTP.
6. Remove only fixture-owned containers, volumes, images and cluster, including
   failed runs. Supervisor shutdown permits the full production stop budget;
   bounded fallback removal rechecks exact Environment, Session and image
   identity. A paused orphan/unrelated-item probe exercises this cleanup, and
   an uncertain shutdown aborts backup. Commands are bounded, credentials are passed through private
   environment/stdin, and no key or plaintext backup appears in logs.

## Scope and evidence

Default package tests remain offline. `make test-kubernetes` explicitly requires
Docker, kind 0.33.0, kubectl 1.37.0 and Helm 4.3.0 and fails if unavailable.
A required CI layer runs the same independent test without provider credentials.
Runtime/package/HTTP/SDK durability tests remain separate; this test exercises
installation and recovery rather than duplicating every contract assertion.

This proves one single-node kind topology on Kubernetes 1.37.0 and same-release
quiesced logical restore. It does not prove production HA, live snapshots,
rolling Workflow upgrades, cross-version upgrades/rollback or a cloud sandbox.
Sandbox workspace volumes are operator-owned and are preserved independently;
the restore does not pretend PostgreSQL/S3 reconstruct them. Real model smoke
remains the separately configured maintainer tier.

Current Mango source, chart, OpenAPI and existing recovery tests define the
acceptance. Standard kind/Compose isolation and PostgreSQL logical backup are
operational references. CMA is a hosted control plane and supplies no analogous
operator database backup or chart lifecycle to inherit.
