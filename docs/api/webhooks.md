---
title: Webhooks
slug: /api/webhooks
---

# Webhooks

Webhooks deliver signed, thin lifecycle notifications to an application-owned
public HTTPS endpoint. They are useful for starting application work promptly;
the Session event log and resource APIs remain the durable source of truth.

The Webhook API and worker delivery use the same operator keyring as Vaults.
Set `MANGO_VAULT_KEYRING_FILE` on both `serve` and `orchestrate`. Without it,
Webhook management returns `422` and the worker does not deliver pending
notifications.

## Manage endpoints

```http
POST   /v1/webhooks
GET    /v1/webhooks
GET    /v1/webhooks/{webhook_id}
POST   /v1/webhooks/{webhook_id}
DELETE /v1/webhooks/{webhook_id}
POST   /v1/webhooks/{webhook_id}/regenerate_signing_secret
```

Create an endpoint with the lifecycle events it should receive:

```json
{
  "url": "https://example.com/mango/events",
  "event_types": [
    "session.status_idled",
    "session.status_terminated",
    "deployment_run.failed"
  ]
}
```

The URL must use HTTPS, port 443, no credentials or fragment, and a hostname
rather than a literal IP or `localhost`. The worker resolves the hostname again
at connect time and permits only public addresses, so DNS changes cannot turn a
previously accepted endpoint into private-network access.

Create returns a `whsec_...` `signing_secret` once. It is encrypted in
PostgreSQL with the operator keyring and omitted from get, list, and update
responses. Store it in the receiving application's secret manager. Rotation
returns the new value once and uses it for newly claimed attempts; it does not
create a dual-secret grace window. An attempt already claimed by a worker may
finish with the previous secret, so receivers should coordinate that brief
in-flight boundary while switching secrets.

Update can replace `url` or `event_types`, or set `status` to `enabled` or
`disabled`. Events emitted while an endpoint is disabled are not queued or
replayed. Re-enabling starts with future events.

## Payload and signature

Mango sends the exact persisted JSON bytes in a `POST` request:

```json
{
  "type": "event",
  "id": "whe_...",
  "created_at": "2026-08-24T10:30:00Z",
  "data": {
    "type": "session.status_idled",
    "id": "sesn_...",
    "workspace_id": "wrkspc_..."
  }
}
```

The payload is intentionally thin. Switch on `data.type`, then fetch the
Session or Deployment Run by `data.id`. Thread lifecycle notifications keep the
Session ID in `data.id` and add `data.session_thread_id`. Mango has no hosted
Organization resource, so it does not manufacture an `organization_id`.

Each attempt uses the [Standard Webhooks](https://www.standardwebhooks.com/)
HMAC-SHA256 convention:

```http
webhook-id: whe_...
webhook-timestamp: 1787567400
webhook-signature: v1,<standard-base64 HMAC>
```

The signed input is `webhook-id + "." + webhook-timestamp + "." + raw_body`.
Use a Standard Webhooks verification library where available, verify the raw
request body before JSON decoding, enforce timestamp freshness, and deduplicate
on `webhook-id`. `created_at` is the event occurrence time; each retry gets a
fresh `webhook-timestamp` and signature while retaining the same ID and body.

## Events in this slice

| Event | When it is emitted |
| --- | --- |
| `session.status_scheduled` | A Session is durably created and ready for input |
| `session.status_run_started` | A Session transitions into a running turn |
| `session.status_idled` | A Session becomes idle, including client-action and budget waits |
| `session.status_rescheduled` | A transient failure schedules another attempt |
| `session.status_terminated` | A Session terminates on completion or error |
| `session.budget_reached` | The shared Session budget pauses execution |
| `session.thread_created` | The coordinator creates an ordinary child or Advisor Thread |
| `session.thread_idled` | A child Thread becomes idle |
| `session.thread_terminated` | A child Thread terminates |
| `session.outcome_evaluation_ended` | An Outcome evaluation finishes |
| `session.updated` | Mutable Session fields change |
| `session.archived` | A Session is archived for the first time |
| `session.deleted` | Prepared Session deletion commits permanently |
| `deployment_run.succeeded` | A scheduled Deployment Run creates its Session |
| `deployment_run.failed` | A scheduled Deployment Run records a failure |

Manual Deployment Runs do not emit `deployment_run.*` events. Mango does not
yet expose `deployment_run.started`: its current immutable Run record commits
with the final Session-creation result, so adding an in-progress lifecycle is a
separate Deployment design change rather than a synthetic notification.

## Delivery behavior

- Subscription membership is snapshotted in the same PostgreSQL transaction as
  the lifecycle change. Adding a subscription later never backfills an event.
- Delivery is at least once and unordered. A receiver can accept an event and
  still see it again if the worker loses its acknowledgement.
- Any `2xx` acknowledges an attempt. Other responses or connection failures
  retry up to three total attempts with jittered exponential delays bounded to
  5–120 seconds; the event is then dropped.
- Redirects are never followed. A `3xx`, or resolution to a non-public address,
  disables the endpoint immediately and fails its pending deliveries.
- One later `2xx` resets the tracked continuous-failure window. Mango records
  this window but does not yet auto-disable solely by duration because CMA does
  not publish the threshold and Mango has not selected an operator policy.
- Terminal delivery records and their payloads are retained internally for 30
  days, then removed in bounded worker cleanup batches. They are not currently
  exposed as a delivery-log API.

Webhooks are notifications, not an infinite durable log. Consumers that cannot
miss a state change should reconcile by fetching resources or reading Session
history in addition to processing Webhooks.
