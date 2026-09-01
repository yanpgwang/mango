---
title: Agents
---

# Agents

Agents are versioned definitions. Sessions resolve an agent version once and
store an immutable snapshot.

## SDK and HTTP example

This excerpt uses the client and resources from [Getting started](../getting-started.md).
Select your language; the wire contract and lifecycle rules follow below.

::include[../../sdk/typescript/examples/quickstart.ts#agent]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../../sdk/python/examples/quickstart.py#agent]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../../sdk/go/examples/quickstart/main.go#agent]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../../examples/sdk-quickstart.sh#agent]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Create an agent

`POST /v1/agents`

```json
{
  "name": "Repository assistant",
  "model": "claude-model-id",
  "system": "Work carefully and explain changes.",
  "description": "Helps maintain a repository",
  "tools": [],
  "mcp_servers": [],
  "skills": [],
  "metadata": {"team": "platform"}
}
```

Required fields:

- `name`: non-empty string;
- `model`: model ID string or an object with a non-empty `id`.

The object model form also preserves supported `effort` and `speed` values.
`effort` accepts either a level string such as `"high"` or the tagged object
`{"type":"high"}`; responses use the tagged object form.
Optional collection and metadata fields may be omitted or supplied with their
documented array/object shape; explicit `null` is not a create-time default.

Coordinators declare a roster with the documented `multiagent` topology:

```json
{
  "type": "coordinator",
  "agents": [
    "agent_latest",
    {"type": "agent", "id": "agent_pinned", "version": 3},
    {"type": "self"}
  ]
}
```

The roster contains 1–20 distinct Agents. An ID string and an Agent object
without `version` resolve to the latest active Version at write time; an object
with `version` selects that exact Version. `self` resolves to the coordinator
Version being written and may appear at most once. Responses and stored Agent
history contain only concrete `{"type":"agent","id","version"}` references,
so later updates to a referenced Agent do not change an existing coordinator.
Creating a Session expands those pins into the full immutable definitions
returned in `session.agent.multiagent.agents`; child Threads will execute those
Session-owned snapshots rather than re-resolving Agent resources.
Archived, missing, duplicate, and nested coordinator references are rejected.
Ordinary roster entries can execute as persistent child Session Threads with
independent context, events, usage, and Workflow state. See the
[multi-agent guide](../guides/multi-agent.md) for an end-to-end example and
[Session Threads](session-threads.md) for the public observation API.

Mango also supports one optional `advisor` roster entry:

```json
{"type":"advisor","model":"claude-opus-5"}
```

It must use that exact shape, appears last in stored responses, and has the
reserved runtime identity `anthropic.advisor`. Mango rejects duplicate Advisor
entries and ordinary Agents with that name. The Advisor is available only to
the primary Agent; it does not appear in `list_agents` and cannot be targeted by
`send_to_agent`. Mango exposes it to the executor as an ordinary no-argument
client tool, runs the configured Advisor model in a separate tool-free request,
and returns the review through an ordinary tool result. The implementation is
provider-neutral. Ordinary child limits such as `max_uses` and `max_tokens` do
not apply to this roster variant.

The current implementation contains a cleanup path for opaque `multiagent`
objects written by earlier development checkouts. This is not a supported
compatibility contract and may be removed directly from `/v1`. While it exists,
an unresolved value is read-only: replace or clear `multiagent` before changing
the Agent or creating a new Session.

Custom Skills use the documented tagged reference:

```json
{"type": "custom", "skill_id": "skill_...", "version": "latest"}
```

`version` may be omitted or set to `latest`. Mango validates the Skill and
stores the concrete immutable Version in the Agent response and version
history. Updating unrelated Agent fields preserves that pin; replacing
`skills` resolves the replacement list again. The latest active Agent Version
also holds a relational retention pin, so its Skill archive cannot be deleted
until the list is replaced or the Agent is archived. External managed-catalog
references return `422` because Mango does not mirror those archives.

The current implementation can also read an untagged Skill value written by an
earlier development checkout. This cleanup branch is not a compatibility
promise and may be removed directly from `/v1`. While it exists, the value is
read-only and must be replaced with a current custom reference before the Agent
can start a new Session.

A successful create returns `200` and version `1`.

## Get and list

```http
GET /v1/agents/{id}
GET /v1/agents
GET /v1/agents/{id}/versions
```

`GET /v1/agents/{id}` returns the latest version. The versions route returns
stored versions in ascending version order. It supports the documented `limit`
and opaque `page` parameters; `limit` defaults to `20` and has a maximum of
`100`. Its response contains `data` and nullable `next_page` fields.

The Agent list supports the documented `created_at[gte]`, `created_at[lte]`,
`include_archived`, `limit`, and `page` parameters. `limit` defaults to `20` and
has a maximum of `100`. Results contain only the latest version of each Agent,
are ordered newest-first by a stable `(created_at, id)` key, and use a
forward-only opaque `next_page` cursor.

## Update

`POST /v1/agents/{id}`

Updates create a new version only when a material field changes:

```json
{
  "version": 1,
  "system": "Use short answers.",
  "metadata": {
    "team": "developer-experience",
    "obsolete_key": null
  }
}
```

When supplied, `version` is an optimistic concurrency check. A stale version
returns `409`.

Field behavior:

- omitted fields preserve the current value;
- `system` and `description` accept `null` to clear;
- `tools`, `mcp_servers`, and `skills` replace the whole list and accept
  `null` to clear;
- `multiagent` replaces and re-resolves the complete roster, and accepts `null`
  to clear;
- metadata keys patch the map, and a `null` value removes a key;
- `name`, `version`, and the `metadata` object itself cannot be `null`;
- `model` may be replaced but cannot be `null`.

An update with no material change returns the current version.

## Archive

`POST /v1/agents/{id}/archive`

Archive is idempotent and does not create a new version. An archived agent is
read-only and cannot be selected for a new session. Existing sessions keep
their stored snapshot.

## Response shape

```json
{
  "id": "agent_...",
  "type": "agent",
  "version": 1,
  "name": "Repository assistant",
  "model": {"id": "claude-model-id"},
  "system": "Work carefully and explain changes.",
  "description": "Helps maintain a repository",
  "tools": [],
  "mcp_servers": [],
  "skills": [],
  "multiagent": null,
  "metadata": {"team": "platform"},
  "created_at": "2026-07-27T00:00:00Z",
  "updated_at": "2026-07-27T00:00:00Z",
  "archived_at": null
}
```

Custom Skill references are validated and version-pinned. OpenSandbox Sessions
materialize those pins and expose a private `Skill` dispatcher that loads the
selected `SKILL.md` into the conversation on demand. The permission-hardened
sandbox-local copy is reconciled while its canonical archive remains immutable
in Mango storage. Agents with Skills must enable `read` for
referenced files, and may not define a custom tool named `Skill`. Accepted Agent
tool and MCP shapes are validated before a version is stored.
