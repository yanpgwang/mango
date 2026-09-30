---
title: Coding agent demo
description: An executable debugging workflow with operator-owned files.
---

# Coding agent demo

A Mango user should be able to hand an agent failing Python code, watch it run
and fix tests, continue the same Session after its worker stops, and retrieve
verified results. The tutorial uses the first-party Go SDK and the standalone
Docker supervisor against a running deployment and real model.

Acceptance criteria:

- Independently authored fixture starts with failing tests; the agent repairs
  the implementation using built-in tools in Docker.
- Inputs travel through Workspace Files upload/download into an operator-owned
  Session directory. The supervisor accepts `--workspace-root`; it binds only
  `<root>/<session-id>` at `/workspace`. Existing named volumes remain the default.
- The application subscribes before sending each message, streams durable events,
  and falls back to paginated history after a disconnect without resending input.
- Stop the first supervisor, start another, and send a second message to the same
  Session. Its code and event history survive the worker replacement.
- Independently run pristine tests against the final code in a fresh container,
  upload the selected result to Files, download it and compare bytes. Save the
  source, test output, event history and resource IDs locally.
- Archive created Session, Agent and Environment, stop the supervisor, and delete
  tutorial File objects even on failure. Retain local artifacts for inspection.

The bind directory is explicitly operator-selected and must already exist on
the Docker daemon host with permissions for the configured container user.
Workers on multiple hosts need the same storage path and contents. Mango does
not create, chown, copy or delete these operator directories. Each activation
uses the same directory; reclaim fencing and graceful shutdown remain unchanged.
This tutorial requires a local Docker daemon (Docker Desktop is supported).

Non-goals: GitHub integration, automatic resource mounts or output publication,
new public API/storage fields, multi-agent orchestration, and new access-control
or operations features. No hosted agent endpoint or external SDK is executed.
See the paired research and intentional adaptations in [provenance](../provenance.md).
