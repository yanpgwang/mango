---
title: Deployments and Deployment Runs
description: Schedule Sessions, run them on demand, and inspect their Run history.
slug: /api/deployments
---

# Deployments and Deployment Runs

A Deployment is a durable template for creating autonomous Sessions. It pins a
specific Agent Version and stores the Environment, initial events, resources,
ordered Vault references, metadata, an optional Session budget template, and an
optional cron schedule used for each Run.

## Create and inspect

```http
POST /v1/deployments
GET /v1/deployments/{deployment_id}
GET /v1/deployments
```

The `agent` field accepts an Agent ID or an explicit Agent reference. An omitted
version resolves to the latest active Version at create time; later Agent
updates do not silently change the Deployment.

```json
{
  "agent": "agent_...",
  "environment_id": "env_...",
  "name": "Nightly knowledge review",
  "initial_events": [
    {
      "type": "user.message",
      "content": [{"type": "text", "text": "Review the project decisions."}]
    }
  ],
  "resources": [
    {"type": "memory_store", "memory_store_id": "memstore_...", "access": "read_write"}
  ],
  "vault_ids": ["vlt_..."],
  "schedule": {
    "type": "cron",
    "expression": "0 2 * * *",
    "timezone": "America/Los_Angeles"
  }
}
```

`initial_events` contains between one and 50 `user.message`,
`user.define_outcome`, or companion `system.message` events; a system message
must immediately follow the user event it annotates. Schedules use five-field
POSIX cron syntax and an IANA timezone. The response includes the next five
occurrences in `schedule.upcoming_runs_at`.

Self-hosted Deployments accept only Memory Store resources. Repository checkout,
input staging, and output retrieval are responsibilities of the Environment
worker that claims each Run. Updating a Deployment affects only future Runs.

`budget: null` explicitly stores no Session spend ceiling. A non-null limit uses
the same integer-USD-cent shape and model-price validation as direct Session
creation. Each Run copies the Deployment's current budget into its new Session;
existing Run Sessions are unaffected by later Deployment updates.

Deployment lists support `agent_id`, `status`, `include_archived`,
`created_at[gte]`, `created_at[lte]`, `limit`, and a forward-only opaque `page`
cursor.

## Update and lifecycle

```http
POST /v1/deployments/{deployment_id}
POST /v1/deployments/{deployment_id}/pause
POST /v1/deployments/{deployment_id}/unpause
POST /v1/deployments/{deployment_id}/archive
```

Update can replace the Agent pin, Environment, initial events, resources,
Vaults, or schedule. Metadata is a per-key patch; a null value deletes one key.
Setting `schedule` to null removes the schedule.
Setting `budget` to null clears the template, while a non-null value replaces
it for future Runs.

Pause suppresses scheduled triggers but does not prevent a manual Run. Unpause
resumes with the next future occurrence and does not backfill missed times.
Archive is idempotent and terminal: an archived Deployment cannot be updated,
unpaused, or run.

## Run and inspect history

```http
POST /v1/deployments/{deployment_id}/run
GET /v1/deployment_runs/{deployment_run_id}
GET /v1/deployment_runs
```

Every attempt creates an immutable Deployment Run. A successful Run contains a
`session_id`; the Session exposes the same parent `deployment_id`. Session and
Run creation commit atomically, so clients never observe only half of a
successful attempt. If Session creation is rejected, the Run instead contains
an `error` and no Session ID. Fatal scheduled errors also pause the Deployment
with an error reason.

Each Run uses the same capability admission as direct Session creation.
Creating the Deployment template does not establish that its runtime
capabilities are available. A manual Run records a creation failure without
pausing the Deployment; a scheduled Run treats a permanent capability failure
as terminal for that schedule and pauses further attempts. Custom Skill pins
are supported by the self-hosted worker and are prepared from each Run's frozen
Session snapshot.

Run lists support `deployment_id`, `has_error`, `trigger_type`, all four
`created_at` bounds, `limit`, and a forward-only opaque `page` cursor. The
`trigger_context` distinguishes manual attempts from scheduled attempts and
records `scheduled_at` for the latter.

Webhook endpoints may subscribe to `deployment_run.succeeded` and
`deployment_run.failed`. These notifications apply only to scheduled Runs;
manual Runs do not emit `deployment_run.*`. See [Webhooks](webhooks.md).

## Scheduling and capabilities

The `orchestrate` worker role executes schedules. Due occurrences are claimed
in small concurrent batches with token-fenced, renewable PostgreSQL leases. A
lost claim cancels its in-flight admission, while a unique
Deployment/occurrence key remains the final commit fence. Running only the API
`serve` role exposes the HTTP surface but does not execute scheduled work.

Deployment resources support Memory Store templates. File-backed outcome
rubrics require configured Files storage but do not require a workspace mount.
Vault references require the configured Vault keyring. File and Git workspace
staging belongs to the operator's launcher. Scheduler jitter and automatic
Deployment archival when an Agent is archived are not implemented.

See [capabilities and limits](../capabilities.md) for the current support boundary.
