---
title: Deployments and Deployment Runs
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
  "name": "Nightly repository audit",
  "initial_events": [
    {
      "type": "user.message",
      "content": [{"type": "text", "text": "Audit the attached inputs."}]
    }
  ],
  "resources": [
    {"type": "file", "file_id": "file_...", "mount_path": "/inputs/source.zip"},
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

A File-backed outcome rubric remains a File reference in the Deployment
template. Each Run resolves and snapshots the current ready top-level File while
creating its Session. Deleting the source cannot affect an already admitted Run,
but a later Run records `file_not_found_error` instead of creating a Session.

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

Run lists support `deployment_id`, `has_error`, `trigger_type`, all four
`created_at` bounds, `limit`, and a forward-only opaque `page` cursor. The
`trigger_context` distinguishes manual attempts from scheduled attempts and
records `scheduled_at` for the latter.

Webhook endpoints may subscribe to `deployment_run.succeeded` and
`deployment_run.failed`. These notifications apply only to scheduled Runs;
manual Runs do not emit `deployment_run.*`. See [Webhooks](webhooks.md).

## Scheduling and capabilities

The `orchestrate` worker role executes schedules. Due occurrences are claimed
with expiring PostgreSQL leases, and a unique Deployment/occurrence key makes a
recovered claim idempotent. Running only the API `serve` role exposes the HTTP
surface but does not execute scheduled work.

File and Memory Store resources require their existing Session sandbox
capabilities. File-backed outcome rubrics require configured Files storage but
do not require a sandbox mount capability. Vault references require the
configured Vault keyring. Git repository resources are rejected explicitly
because the current repository snapshot contract is selected only for direct
create-time Sessions; repeat-run refresh/freeze semantics have not been defined
for Deployment templates. Scheduler jitter and automatic Deployment archival
when an Agent is archived are not implemented.

See [capabilities and limits](../capabilities.md) for the current support boundary.
