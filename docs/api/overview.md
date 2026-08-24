---
title: API overview
slug: /api
---

# API overview

The server exposes Mango's current Agent, Environment, Session, Event, File,
Skill, Memory, Vault, Webhook, Deployment, Environment Work, and Session Thread HTTP
surface under `/v1`. Operation presence does not imply unrestricted support
for every workflow; resource-specific limitations are documented explicitly.

:::info

This reference documents repository behavior. See
[capabilities and limits](../capabilities.md) for what is supported, limited,
or still in preview. Mango's API is not defined by a third-party SDK.

:::

## Endpoints

| Resource | Endpoints |
| --- | --- |
| Agents | `POST/GET /v1/agents`, `GET/POST /v1/agents/{id}`, versions, archive |
| Environments | `POST/GET /v1/environments`, get, update, archive, delete |
| Environment Work | Get/update/list/Ack/Heartbeat/Poll/Stats/Stop under `/v1/environments/{id}/work`; consumed by self-hosted workers |
| Sessions | `POST/GET /v1/sessions`, get, update, archive, delete |
| Events | `POST/GET /v1/sessions/{id}/events`, SSE stream |
| Session Threads | List/get/archive Threads; list and stream one Thread's events |
| Files | `POST/GET /v1/files`, metadata, content download, delete |
| Skills | Create/list/get/delete custom Skills and immutable Versions; download Version zip archives |
| Memory | Create/list/get/update/archive/delete Stores; create/list/get/update/delete Memories; get/list/redact immutable Versions |
| Vaults | Create/list/get/update/archive/delete Vaults; create/list/get/update/archive/delete encrypted Credentials; validate MCP OAuth Credentials |
| Webhooks | Create/list/get/update/delete signed lifecycle endpoints; rotate one-time signing secrets |
| Deployments | Create/list/get/update/archive/pause/unpause/run under `/v1/deployments`; get/list immutable records under `/v1/deployment_runs` |
| Session Resources | Add, list, get, update contract, and delete under `/v1/sessions/{id}/resources` |
| Operations | `GET /healthz`, `GET /readyz`, `GET /openapi.yaml` |

Resource-specific request shapes are covered in:

- [Agents](agents.md)
- [Environments](environments.md)
- [Environment Work](environment-work.md)
- [Sessions](sessions.md)
- [Events and streaming](events.md)
- [Session Threads](session-threads.md)
- [Session Resources](session-resources.md)
- [Files](files.md)
- [Skills](skills.md)
- [Memory](memory.md)
- [Vaults and Credentials](vaults.md)
- [Webhooks](webhooks.md)
- [Deployments and Deployment Runs](deployments.md)

## Headers

Every protected route requires an API key. The default development stack uses
`sk-mango-local-development`. Send it as a standard bearer credential:

```http
authorization: Bearer sk-mango-local-development
content-type: application/json
```

Every non-empty JSON request body requires `content-type: application/json`.
File and Skill uploads instead require `multipart/form-data`; File uploads are
limited to 500 MB and Skill bundles must be smaller than 30 MB. Mango does not
use provider version or beta headers on its inbound API.

Each bearer key resolves to exactly one Workspace, and every key for that
Workspace can access the same resources. Workspace IDs are not added to public
request or response bodies.

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

Errors use Mango's current error envelope, whose shape is retained from the
original `/v1` design:

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
| `404` | `not_found_error` |
| `409` | `conflict_error` |
| `412` | `precondition_failed_error` |
| `413` | `request_too_large` |
| `422` | `invalid_request_error` |
| `500` | `api_error` |

A failed Memory SHA-256 precondition is the more specific
`409 memory_precondition_failed_error`.

These mappings are Mango's current public contract. See
[capabilities and limits](../capabilities.md) for behavioral boundaries.

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

Repository tests keep local references resolvable and lock the intended
operation inventory and event unions. During alpha, Mango may change that
inventory in place when the implementation, OpenAPI, documentation, and tests
move together for a clear product reason.
