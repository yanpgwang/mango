---
title: Build a coding-agent iteration loop
slug: /guides/coding-agent-iterate
---

# Build a coding-agent iteration loop

This guide shows how Mango represents a common coding-agent job: receive source
files and failing checks, work on a writable copy until the checks pass, and
publish the repaired source as a durable Session output.

It is a resource and lifecycle walkthrough, not a separate launcher. Complete
[Getting started](../getting-started.md) first and use the HTTP operations in
the [API reference](../api/overview.md) from your own client or application.

## Resource flow

1. Upload the source and checks as File objects.
2. Create a cloud Environment. The Mango operator chooses the sandbox provider;
   the Agent-facing workflow does not depend on Docker or a hosted sandbox API.
3. Create an Agent with the local coding tools it needs.
4. Create a Session and attach the Files as immutable Session Resources.
5. Ask the Agent to copy those inputs into its writable workspace, observe the
   failing checks, fix the source, and write the verified result under
   `/mnt/session/outputs`.
6. Follow the Session event stream until the durable idle boundary, then list
   and download the published output File.

The relevant Agent tool policy can stay narrow for an offline repair task:

```json
{
  "model": "your-model-id",
  "tools": [{
    "type": "agent_toolset_20260401",
    "default_config": {
      "enabled": false,
      "permission_policy": {"type": "always_allow"}
    },
    "configs": [
      {"name": "bash", "enabled": true, "permission_policy": {"type": "always_allow"}},
      {"name": "read", "enabled": true, "permission_policy": {"type": "always_allow"}},
      {"name": "write", "enabled": true, "permission_policy": {"type": "always_allow"}},
      {"name": "edit", "enabled": true, "permission_policy": {"type": "always_allow"}},
      {"name": "glob", "enabled": true, "permission_policy": {"type": "always_allow"}},
      {"name": "grep", "enabled": true, "permission_policy": {"type": "always_allow"}}
    ]
  }]
}
```

Attach each input at an explicit read-only upload path when creating the
Session:

```json
{
  "agent": "agent_...",
  "environment_id": "env_...",
  "resources": [
    {"type": "file", "file_id": "file_source", "mount_path": "/mnt/session/uploads/calc.py"},
    {"type": "file", "file_id": "file_checks", "mount_path": "/mnt/session/uploads/test_calc.py"}
  ]
}
```

The task should name the acceptance outcome, not prescribe a provider-specific
sandbox implementation. For example:

> Copy the uploaded files into your writable workspace. Run the checks first so
> you observe the current failure, repair the implementation, and rerun the
> checks until they pass. Write the verified `calc.py` to
> `/mnt/session/outputs/calc.py`.

Files under `/mnt/session/uploads` remain immutable. Work happens elsewhere in
the Session sandbox. A file written under `/mnt/session/outputs` is published
through Mango's File lifecycle and can be listed by the Session scope after the
turn reaches idle.

## Executable verification

Mango keeps the automation in its test suite instead of duplicating the full
service startup and polling logic in this guide:

```bash
# Retry-safe deterministic model; part of the ordinary service suite.
make test-coding-agent

# Same durable outcome against the configured, potentially billable model.
scripts/with-dev-env make test-coding-agent-live
```

Both variants exercise PostgreSQL, Temporal, Docker sandbox execution, File
Resources, tool results, independent output verification, and Session Output
publication. The deterministic scenario additionally proves that the Agent
observed a failing check before repairing it.

## Design boundary

The small broken-calculator scenario is adapted from Anthropic's public CMA
iterate cookbook. Mango adopts the useful user problem and test outcome, but
the API, persistence, events, sandbox selection, and output lifecycle are
Mango-owned. See [Design provenance](../provenance.md) for the exact source and
the adopted, changed, and rejected parts.
