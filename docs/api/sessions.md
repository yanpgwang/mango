---
title: Sessions
---

# Sessions

Sessions bind an immutable agent snapshot to an environment and own an
append-only event history.

## SDK and HTTP example

This excerpt uses the client and resources from [Getting started](../getting-started.md).
Select your language; the wire contract and lifecycle rules follow below.

::include[../../sdk/typescript/examples/quickstart.ts#session]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../../sdk/python/examples/quickstart.py#session]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../../sdk/go/examples/quickstart/main.go#session]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../../examples/sdk-quickstart.sh#session]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Create

`POST /v1/sessions`

The `agent` field supports three forms.

Latest version:

```json
{
  "agent": "agent_...",
  "environment_id": "env_...",
  "title": "Investigation"
}
```

Pinned version:

```json
{
  "agent": {"type": "agent", "id": "agent_...", "version": 2},
  "environment_id": "env_..."
}
```

Session-local overrides:

```json
{
  "agent": {
    "type": "agent_with_overrides",
    "id": "agent_...",
    "version": 2,
    "system": "Focus on production incidents.",
    "tools": []
  },
  "environment_id": "env_..."
}
```

Overrides replace model, system, tools, MCP servers, or skills for this session
only. They do not mutate or renumber the agent. A model override may change the
model ID or speed; effort remains an Agent-level setting and a session override
does not replace it. Overrides also apply to `self` copies in a coordinator
roster. Independently referenced Agents are unaffected.

For a coordinator, `session.agent.multiagent.agents` expands the Agent
resource's Version references into full immutable Agent definitions. The
definitions are captured with the Session, in roster order, rather than loaded
again when a child Thread starts. A `self` definition reflects the effective
Session overrides and every roster member omits its own `multiagent` field,
preserving the one-level topology. Existing Sessions keep those snapshots even
if a referenced Agent is later updated or archived.

The effective custom Skill list is revalidated when the Session is created.
Every omitted or `latest` value is replaced by a concrete immutable Version in
the returned `session.agent.skills` snapshot. PostgreSQL pins those Versions in
the same transaction as the Session; deleting a pinned Version is rejected
until the Session is physically deleted. The pin migration backfills concrete,
still-ready custom references from existing Session snapshots; former opaque
values remain readable but are not treated as executable references. Up to 500
Skills are accepted, subject to a 500 MB aggregate expanded-size limit and
unique runtime names. Docker, E2B, CubeSandbox, OpenSandbox, and Daytona verify
pinned archives and expose them at `/workspace/skills/<name>/`; external roster
Agents use isolated subdirectories below `/workspace/skills/.agents/`. Docker
enforces a read-only bind mount, while remote providers expose a
permission-hardened sandbox-local copy and reject ordinary file-tool writes to
the Skill root. The model first receives every Skill name plus
descriptions bounded to one percent of the configured context window.
When it invokes the private `Skill` dispatcher, the runtime returns
`Launching skill: <name>` and injects the complete selected `SKILL.md`, prefixed
with its base directory, into the provider conversation. Referenced supporting
files remain on disk for ordinary `read` or `bash` access. Sessions using an
incapable sandbox adapter or a self-hosted Environment reject custom Skills at
creation with `422`, before any Session, Work item, or execution wakeup is created. This
check covers the effective primary Agent and every resolved roster member
after overrides, including a `self` copy. See
[Sandbox backends](../sandboxes.md#custom-skill-mounts) for the remote-copy
limitation.

Optional `initial_events` may contain up to 50 `user.message` or
`user.define_outcome` objects. A non-empty list starts execution immediately.
An outcome may use either an inline text rubric or a ready top-level File rubric
from the same Workspace. File text is validated and snapshotted before the
Session admission transaction, so a missing or invalid File cannot leave a
partially created Session and deleting it later cannot change the active
outcome.
The optional `title`, `metadata`, `initial_events`, `resources`, and `vault_ids`
fields must use their documented non-null shapes when present; omission supplies
the empty/default value. `budget: null` explicitly selects no spend ceiling. A
non-null budget sets a Session-wide ceiling in integer USD cents:

```json
{
  "budget": {
    "type": "limit",
    "max_list_cost": {"amount": "2500", "currency": "USD"}
  }
}
```

Budgeted Sessions require model IDs present in Mango's built-in price catalog,
which currently contains canonical Anthropic model IDs and published list
prices, for the coordinator and every resolved roster member. A router may
still forward those requests, but an opaque router-defined model alias is not
assigned a guessed price.
`resources` accepts File inputs, public Git repository snapshots, and up to
eight Memory Store inputs when the corresponding sandbox capability is
configured:

```json
{
  "type": "file",
  "file_id": "file_...",
  "mount_path": "/reports/input.csv"
}
```

Each attachment creates an independent, downloadable Session-scoped File copy
beneath `/mnt/session/uploads`. Docker presents it read-only; E2B, CubeSandbox,
OpenSandbox, and Daytona currently expose a writable sandbox-local copy. A Memory Store input
uses `type: "memory_store"`, `memory_store_id`, optional `instructions`, and
`read_write` or `read_only` access; it is mounted beneath `/mnt/memory` and can
only be attached at creation. A `git_repository` input uses an anonymous HTTPS
`url`, optional branch-or-commit `checkout`, and optional `/workspace` child
`mount_path`; Mango freezes and returns its exact `resolved_commit`. Git
repositories are create-time-only on Docker, E2B, CubeSandbox, OpenSandbox,
and Daytona. Self-hosted Environments reject these resource attachments with `422`.
E2B and Cube currently buffer File and repository archive transfers in worker
memory during materialization.
`vault_ids` is an ordered list of active Vault references. The order is frozen
with the Session: for an MCP endpoint, the first Vault containing a matching
credential wins. Admission requires the Vault keyring to be configured and
rejects missing, archived, empty, or duplicate references. Updating
`vault_ids` after creation remains unsupported by the current Mango contract.

## Get and update

```http
GET /v1/sessions/{id}
POST /v1/sessions/{id}
```

The update body accepts `agent`, `metadata`, `title`, and `budget`:

```json
{
  "title": "New title",
  "metadata": {"owner": "sre", "stale": null},
  "agent": {
    "tools": [
      {"type": "agent_toolset_20260401"},
      {"type": "mcp_toolset", "mcp_server_name": "linear"}
    ],
    "mcp_servers": [
      {"type": "url", "name": "linear", "url": "https://mcp.linear.app/sse"}
    ]
  }
}
```

- `metadata` is a per-key patch: a string upserts the key, `null` deletes it,
  and omitting the field preserves the whole bag.
- `title` may be omitted or set to a string; `null` is not a no-op update.
- `agent` updates only `tools` and `mcp_servers`, as a full replacement: the
  array you send becomes the new value, `[]` clears, and omitting preserves.
  `model`, `system`, and `skills` are fixed for the session's lifetime and are
  rejected; set them with `agent_with_overrides` at create time instead.
- An `agent` update is session-local. It never renumbers or mutates the agent
  resource, and it applies from the next turn.
- **An `agent` update requires an `idle` session.** A request that arrives while
  a turn is in flight returns `409`; send an untargeted `user.interrupt` first.
  `title` and `metadata` carry no such precondition.
- `vault_ids` is rejected on update by the current Mango API.
- A Session created with a budget may replace it or set `budget: null` to remove
  it. A changed maximum must be strictly greater than the exact list cost already
  consumed. A Session created without a budget cannot add one later, and a
  removed budget cannot be re-added.

Changed fields and their `session.updated` event commit together. The event
carries only the fields the request actually changed; a request that changes
nothing emits no event.

## List

`GET /v1/sessions`

Supported query parameters:

| Parameter | Meaning |
| --- | --- |
| `limit` | Page size, `1`–`1000`; default `100`. Values above `1000` return a validation error. |
| `order` | `asc` or `desc`; default `desc` |
| `page` | Opaque next or previous cursor |
| `agent_id` | Match agent ID |
| `agent_version` | Match version; requires `agent_id` |
| `statuses[]` | Repeatable public status filter |
| `include_archived` | `true` or `false` |
| `created_at[gt\|gte\|lt\|lte]` | RFC 3339 timestamp bounds |
| `deployment_id` | Match Sessions created by the Deployment |
| `memory_store_id` | Match Sessions attached to the Memory Store |

The response includes both directions:

```json
{
  "data": [],
  "next_page": null,
  "prev_page": null
}
```

## Archive and delete

```http
POST /v1/sessions/{id}/archive
DELETE /v1/sessions/{id}
```

A running session cannot be archived or deleted and returns `409`. A
`user.interrupt` durably cancels active model or tool Activities across API and
worker processes so the Session can return to `idle`. Omitting
`session_thread_id` interrupts every non-archived Thread; providing it targets
only the named Thread.

Archive prevents further input and retains history and Session Files, but does
not release the sandbox. Automatic idle or archive-based sandbox reclamation
is not implemented. Delete performs sandbox cleanup and removes Session-owned
Files; download any outputs you need to retain before deleting a Session.

Delete removes the session and persisted history, sends a final
`session.deleted` event to active subscribers, and closes their streams:

```json
{"id": "session_...", "type": "session_deleted"}
```

## Response notes

The response embeds the resolved agent snapshot and includes nullable `budget`,
`resources`, `vault_ids`, `outcome_evaluations`, `stats`, `usage`, and
`deployment_id`.
`stats` and `usage` are cumulative live projections, and
`outcome_evaluations` reflects each admitted outcome. `resources` embeds active
File and Memory Store Resource objects. Ordered `vault_ids` are resolved at
creation; update-time vault replacement is rejected.
`usage` aggregates provider-reported token, prompt-cache, Web Fetch, and Web
Search counters across every Session Thread. `usage.list_cost` is calculated
from Mango's current built-in price catalog, provider-reported execution facts
that affect that catalog's rates, the Web Search request rate, and $0.08 per
Session active hour, then rounded to the nearest cent for the public monetary
projection. Provider routing is not Agent configuration. Thread list cost
excludes Session runtime. Accounting remains exact internally, and
model-request admission checks the shared ceiling before every request; an
already in-flight request may take the Session over its limit.
Provider-reported tokens remain visible even when a response-level billing rule
makes their list cost zero, such as an unbilled Claude Fable 5 refusal.

When the ceiling is reached, affected Threads become idle with
`budget_reached`. If the whole Session becomes idle, `session.usage` is emitted
immediately before `session.status_idle`. A pending client action takes
precedence as `requires_action`; its result remains admissible, and the budget is
checked before any subsequent model request. Raising or removing the budget
resumes a turn that was paused at that check.
`deployment_id` is null for direct Session creation and contains the parent
Deployment ID for Deployment-created Sessions.

For Mango-managed Docker, E2B, CubeSandbox, OpenSandbox, and Daytona Sessions
with Files storage configured, regular files written beneath `/mnt/session/outputs` are published
before the Session becomes idle. List them with
`GET /v1/files?scope_id={session_id}` and download them through the Files
content endpoint. See [Files](files.md#session-outputs) for limits and provider
constraints.

See [capabilities and limits](../capabilities.md).
