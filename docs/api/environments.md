---
title: Environments
description: Define a self-hosted execution queue and issue worker credentials.
---

# Environments

An Environment is the durable queue and trust boundary used by operator-run
workers. Mango currently supports one configuration:

```json
{
  "name": "Development workers",
  "config": {"type": "self_hosted"}
}
```

Omitting `config` creates the same `self_hosted` Environment. Provider names,
container images, networking policy, packages, and infrastructure credentials
are not Environment fields; those belong to the operator's launcher.

## Create

`POST /v1/environments`

::include[../../sdk/typescript/examples/quickstart.ts#environment]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../../sdk/python/examples/quickstart.py#environment]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../../sdk/go/examples/quickstart/main.go#environment]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../../examples/sdk-quickstart.sh#environment]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

The request accepts `name`, optional `description`, `metadata`, `scope`, and
`config`. `scope` is `organization` by default; `account` is also accepted.
Any config type other than `self_hosted`, or extra config fields, returns an
invalid-request error.

The response includes the Environment ID, scope, normalized config, timestamps,
and archive state.

## Update, archive, and delete

- `POST /v1/environments/{environment_id}` updates mutable metadata, scope, or
  the equivalent `self_hosted` config.
- `POST /v1/environments/{environment_id}/archive` prevents new Session use and
  new Work claims while retaining the resource.
- `DELETE /v1/environments/{environment_id}` removes an unused Environment.

Existing Sessions keep their stored Environment snapshot. Deletion is rejected
while the Environment is referenced.

## Worker credentials

Configure a Workspace API key and this Environment's ID on the supervisor.
Mango has not yet introduced a narrower Environment polling key. The supervisor
polls and acknowledges Environment Work. After Ack, the Work item supplies a
short-lived Session credential for heartbeat, Session events, immutable inputs,
result submission, failure, and Stop.

The first-party Docker launcher keeps the Workspace credential outside Session
containers. See [Environment Work](environment-work.md) and
[Self-hosted worker](../guides/self-hosted-worker.md).
