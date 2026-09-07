---
title: API reference
description: Requests, responses, authentication, and event streams for Mango's HTTP API.
slug: /api
---

# API reference

Send requests to your Mango server under `/v1`. The local stack uses
`http://localhost:8080`. Use a [first-party SDK](../sdk.md) for typed access, or
call the HTTP endpoints directly.

## Endpoints

| Resource | Use it to… | Base path |
| --- | --- | --- |
| [Agents](agents.md) | Define and version an agent's instructions, model, and tools. | `/v1/agents` |
| [Environments](environments.md) | Group Sessions by execution configuration. | `/v1/environments` |
| [Sessions](sessions.md) | Create, steer, inspect, archive, or delete ongoing work. | `/v1/sessions` |
| [Events](events.md) | Send input, return tool results, read history, and stream output. | `/v1/sessions/{id}/events` |
| [Threads](session-threads.md) | Inspect and manage a Session's child conversations. | `/v1/sessions/{id}/threads` |
| [Session Resources](session-resources.md) | Attach Memory Stores when creating Sessions. | `POST /v1/sessions` |
| [Files](files.md) | Upload immutable bytes for supported application and message workflows. | `/v1/files` |
| [Skills](skills.md) | Manage instruction bundles and immutable Versions. | `/v1/skills` |
| [Memory](memory.md) | Store versioned UTF-8 files across Sessions. | `/v1/memory_stores` |
| [Vaults](vaults.md) | Store credentials used by MCP connections. | `/v1/vaults` |
| [Webhooks](webhooks.md) | Subscribe to signed lifecycle notifications. | `/v1/webhooks` |
| [Deployments](deployments.md) | Schedule or manually start Sessions and inspect their Runs. | `/v1/deployments`, `/v1/deployment_runs` |
| [Environment Work](environment-work.md) | Claim and renew leased work for self-hosted execution. | `/v1/environments/{id}/work` |

The public probes are `GET /healthz` and `GET /readyz`; `GET /openapi.yaml`
returns the schema. For execution-path constraints, consult
[capabilities and limits](../capabilities.md).

## Headers

Every ordinary protected route requires an API key. The default development stack uses
`sk-mango-local-development`. Send it as a standard bearer credential:

```http
authorization: Bearer sk-mango-local-development
content-type: application/json
```

Every non-empty JSON request body requires `content-type: application/json`.
File and Skill uploads instead require `multipart/form-data`; File uploads are
limited to 500 MB and Skill bundles must be smaller than 30 MB. Mango does not
use provider version or beta headers on its inbound API.

Each API key resolves to exactly one Workspace, and every API key for that
Workspace can access the same resources. Self-hosted Poll responses are the one
internal exception: their credential payload contains a per-Work Session token
accepted after Ack only by the claimed read/stream, tool-result, lease, and
pinned-input routes. Expiry, Stop, or reclaim revokes the capability. Workspace
IDs are not added to public request or response bodies.

Mango intentionally has no end-user or role model. A surrounding SaaS may map
many users to a Workspace and apply its own RBAC before calling Mango. Use the
operator CLI to manage the OSS boundary:

```sh
mango workspace create -name acme
mango api-key create -workspace wrkspc_... -label production
mango api-key list -workspace wrkspc_...
mango api-key revoke -id key_...
```

Every response includes a `request-id` header. JSON request bodies are limited
to 32 MiB and unknown top-level fields are rejected. File uploads require
configured S3-compatible storage.

## Errors

JSON errors include a type, message, and request ID:

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "name is required"
  },
  "request_id": "req_..."
}
```

| HTTP status | Current error type |
| --- | --- |
| `400` | `invalid_request_error` |
| `401` | `authentication_error` |
| `403` | `permission_error` |
| `404` | `not_found_error` |
| `409` | `conflict_error` |
| `412` | `precondition_failed_error` |
| `413` | `request_too_large` |
| `422` | `invalid_request_error` |
| `500` | `api_error` |

A failed Memory SHA-256 precondition is the more specific
`409 memory_precondition_failed_error`.

## Pagination

Top-level Agent, Environment, Session, and Session Thread lists, Agent version
histories, and Event lists use opaque `page` tokens. Skill and Skill Version lists use the same
forward-only token convention. A cursor is bound to its resource and normalized
filters; Agent and Skill version cursors are additionally bound to their parent
resource ID, and Session cursors are bound to sort order. Reusing a cursor
outside its scope returns `400`.

List responses use `data` and nullable cursor fields:

```json
{
  "data": [],
  "next_page": null
}
```

Session lists also include `prev_page`. Agent, Agent Version, and Environment
lists are forward-only and include `next_page`.

Session Resource lists use a forward-only opaque `page` cursor. Omitting
`limit` returns all resources for the Session, whose active-resource limit is
500.

Files currently use ID-based pagination: `after_id` and
`before_id` select a direction, while the response contains `has_more`,
`first_id`, and `last_id`. The two direction parameters cannot be combined.

Vault, Credential, Webhook, Deployment, Deployment Run, and Environment Work lists use forward-only opaque
`page` cursors and return `data` with nullable `next_page`. Cursors are bound to
their normalized filters; Credential cursors are additionally bound to their
parent Vault ID and archive filter.

## OpenAPI

The running server exposes `/openapi.yaml`, sourced from
`internal/httpapi/openapi.yaml`. It defines Mango's operation IDs, path and
query parameters, request and response schemas, list envelopes, and shared
error responses. The Session Event contract includes the client-submittable
and persisted variants plus ephemeral SSE `event_start` and `event_delta`
preview frames.

Use the [OpenAPI source](https://github.com/yanpgwang/mango/blob/main/internal/httpapi/openapi.yaml)
when generating integrations. The API is alpha and may change on `/v1`; deploy
a client that matches your server checkout.
