---
title: Environments
---

# Environments

An environment is a named session execution configuration.

## SDK and HTTP example

This excerpt uses the client and resources from [Getting started](../getting-started.md).
Select your language; the wire contract and lifecycle rules follow below.

::include[../../sdk/typescript/examples/quickstart.ts#environment]{lang="typescript" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../../sdk/python/examples/quickstart.py#environment]{lang="python" meta='tab="Python" tab-group="mango-language"'}

::include[../../sdk/go/examples/quickstart/main.go#environment]{lang="go" meta='tab="Go" tab-group="mango-language"'}

::include[../../examples/sdk-quickstart.sh#environment]{lang="bash" meta='tab="HTTP" tab-group="mango-language"'}

## Create

`POST /v1/environments`

```json
{
  "name": "local",
  "description": "Default analysis environment",
  "metadata": {"team": "data"},
  "config": {"type": "cloud"}
}
```

`name` is required. If `config.type` is omitted, the stored type defaults to
`cloud`. `description` and `metadata` are optional. `scope` accepts `account` or
`organization` for `self_hosted` environments and is rejected for `cloud`.

Cloud environments accept package lists for `apt`, `cargo`, `gem`, `go`, `npm`,
and `pip`. The first sandbox-using turn installs those packages before the
Session's durable sandbox binding becomes visible. Package names are passed
directly to the corresponding package manager; the caller is responsible for
valid names and versions. Package setup requires an image containing each
requested package manager; the default Python Alpine image does not include
`apt-get` or every supported language runtime.

Limited networking is available when the deployment selects the `opensandbox`
backend. It is deny-by-default and accepts this shape:

```json
{
  "type": "limited",
  "allowed_hosts": ["api.example.com", "*.assets.example.com"],
  "allow_mcp_servers": true,
  "allow_package_managers": false
}
```

`allowed_hosts` entries are bare hostnames or a leading `*.` wildcard. URL
schemes, ports, paths, and embedded wildcards are rejected. The two allow flags
default to `false`. Deployments using Docker, E2B, CubeSandbox, or
Daytona return `422` for a limited policy instead of storing unenforced intent.

The runtime accepts `cloud` and `self_hosted` sessions. In `cloud`, enabled
built-in sandbox tools execute on the configured worker sandbox. In
`self_hosted`, the same `agent.tool_use` parks the Session with
`requires_action`; the client executes it and sends a correlated
`user.tool_result`. The server then resumes the same model loop without
executing the tool a second time.

Web Search and Web Fetch execute through the configured model endpoint in both
Environment types. They are never dispatched to the self-hosted worker and do
not create `user.tool_result` waits. The endpoint must support Mango's current
native Web declarations; otherwise disable those tools. Native Web tools
require `always_allow`: Mango cannot pause a provider request for approval.

`always_ask` also applies to self-hosted shell/file tools. The client first submits
`user.tool_confirmation`; an allow authorizes external execution but does not
resume the model until the correlated `user.tool_result` arrives. A denial
requires no external execution or result. See
[external tool approvals](events.md#approve-externally-executed-tools).

## Get and list

```http
GET /v1/environments/{id}
GET /v1/environments
```

The list supports `include_archived`, `limit`, and the forward-only opaque
`page` cursor. Mango uses a local default limit of `100` and maximum of `1000`
because the public Environment list reference does not specify either bound.
The response is:

```json
{"data": [], "next_page": null}
```

## Archive

`POST /v1/environments/{id}/archive`

Archive is idempotent. Archived environments cannot be used for new sessions,
but existing session references remain intact.

## Update

`POST /v1/environments/{id}` updates `name`, `description`, metadata, explicit
self-hosted `scope`, and the Environment type. Metadata is patched per key;
`null` and empty string delete a key. Changing a self-hosted Environment to
`cloud` clears its inapplicable scope. Archived Environments are read-only.

Unrestricted and limited networking and package lists are accepted when the
selected backend can enforce them. Omitting
`networking` or `packages` from a cloud config update preserves its existing
value. An update affects Sessions created afterward; each Session keeps the
effective Environment configuration it captured at creation. Within a limited
policy update, omitted `allowed_hosts`, `allow_mcp_servers`, and
`allow_package_managers` fields preserve their existing values.

For a limited Session, explicit hosts form the base egress allowlist.
`allow_mcp_servers` adds the host of each MCP URL in the Session's current Agent
snapshot, including a session-local MCP replacement on the next sandbox-using
turn. `allow_package_managers` keeps the canonical public registries available
after provisioning. Native `web_search` and `web_fetch` run outside the sandbox
and are not constrained by its egress policy.

Configured packages can install even when `allow_package_managers` is `false`:
the provisioning path temporarily adds the canonical Debian/Ubuntu, Cargo,
RubyGems, Go, npm, and PyPI registry hosts, installs packages, restores the
final policy, and only then publishes the sandbox binding. The built-in list is
`deb.debian.org`, `security.debian.org`, `archive.ubuntu.com`,
`security.ubuntu.com`, `ports.ubuntu.com`, `snapshot.debian.org`, `crates.io`,
`index.crates.io`, `static.crates.io`, `rubygems.org`,
`index.rubygems.org`, `api.rubygems.org`, `proxy.golang.org`, `sum.golang.org`,
`storage.googleapis.com`, `registry.npmjs.org`, `pypi.org`, and
`files.pythonhosted.org`. Custom indexes and direct Go VCS hosts must be listed
explicitly in `allowed_hosts`.

## Delete

`DELETE /v1/environments/{id}`

An unreferenced environment can be deleted:

```json
{
  "id": "env_...",
  "type": "environment_deleted"
}
```

Deleting an environment referenced by a session returns `409`.

## Response shape

```json
{
  "id": "env_...",
  "type": "environment",
  "name": "local",
  "description": "Default analysis environment",
  "metadata": {"team": "data"},
  "config": {
    "type": "cloud",
    "networking": {"type": "unrestricted"},
    "packages": {
      "type": "packages",
      "apt": [], "cargo": [], "gem": [], "go": [], "npm": [], "pip": []
    }
  },
  "created_at": "2026-07-27T00:00:00Z",
  "updated_at": "2026-07-27T00:00:00Z",
  "archived_at": null
}
```

The default cloud response includes Mango's resolved unrestricted-network and
empty-package defaults. Configured package lists,
description, metadata, and self-hosted scope persist across create, get, list,
update, and archive. A package-manager error prevents sandbox binding and tool
execution; a later retry resumes provisioning from the durable intent. The
selected isolated sandbox image must provide every requested package-manager
binary.
