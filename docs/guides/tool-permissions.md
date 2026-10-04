---
title: Tool permissions
description: Configure per-invocation model evaluation and durable human approval.
---

# Tool permissions

Use `auto` when an Agent should handle routine work and ask you about uncertain
calls. Mango evaluates each invocation before exposing executable Work or
calling a remote MCP tool. This is an opt-in first version; its judgment quality
depends on your configured model. Keep `always_ask` for workflows that require a
human checkpoint before every call.

| Policy | Behavior |
| --- | --- |
| `always_allow` | Execute without a permission-model request. Built-in default. |
| `always_ask` | Wait for a committed human confirmation. MCP default. |
| `auto` | Evaluate the exact tool, arguments, original task, and conversation; then allow, ask, or deny. |

Policies apply to the six self-hosted tools (`bash`, `read`, `write`, `edit`,
`glob`, `grep`) and remote MCP tools. Custom tools belong to your application
and have no Mango permission policy. Provider-native `web_search` and
`web_fetch` execute inside the model request; enabled Web tools require
`always_allow`. Disable them when using a built-in default of `auto`.

## Configure a local tool

Create an Agent with only `read` enabled and automatic evaluation:

```bash
curl -sS "$MANGO_URL/v1/agents" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Repository reader",
    "model": "your-configured-model-id",
    "system": "Inspect requested repository files and explain your findings.",
    "tools": [{
      "type": "agent_toolset_20260401",
      "default_config": {"enabled": false},
      "configs": [{
        "name": "read",
        "enabled": true,
        "permission_policy": {"type": "auto"}
      }]
    }]
  }'
```

Create a Session with that Agent and a self-hosted Environment, start its worker,
and send a task such as `Read README.md and explain how to run the tests.` See
[the self-hosted worker guide](self-hosted-worker.md) for that execution setup.
The sandbox and Files remain self-hosted; the permission request uses the same
configured Messages adapter and model ID as the Agent. Mango's public API does
not call or depend on Claude Managed Agents.

For MCP, configure the policy on its toolset instead:

```json
{
  "tools": [{
    "type": "mcp_toolset",
    "mcp_server_name": "issues",
    "default_config": {"permission_policy": {"type": "auto"}}
  }],
  "mcp_servers": [{
    "type": "url",
    "name": "issues",
    "url": "https://your-mcp-server.example/mcp"
  }]
}
```

Per-tool `configs` can override either default. MCP connectivity and Vault
authentication follow the [existing MCP workflow](../architecture/storage-context-and-tools.md#mcp).

## Handle the recorded outcome

Both `agent.tool_use` and `agent.mcp_tool_use` include `evaluated_permission` and
`evaluation`. For example, an uncertain local call emits:

```json
{
  "id": "sevt_read",
  "type": "agent.tool_use",
  "processed_at": null,
  "name": "read",
  "input": {"path": "report.md"},
  "evaluated_permission": "ask",
  "evaluation": {
    "type": "auto",
    "evaluated_permission": {"type": "ask", "reason_code": "indeterminate"}
  }
}
```

| Recorded outcome | Next step |
| --- | --- |
| `allow` | The self-hosted worker receives ordinary tool Work; Mango executes server-owned MCP calls. |
| `ask` / `indeterminate` | Answer `user.tool_confirmation` with `allow` or `deny`, using the original tool event ID. An approved local call still needs its execution result. |
| `deny` / `high_risk` | Mango returns a matching error tool result to the model and continues the Agent. There is no confirmation override for this invocation. |

The top-level outcome agrees with the nested automatic decision. An allow has
no reason code. Fixed policies instead expose `evaluation: {"type":
"always_allow"}` or `{"type": "always_ask"}`. Private model reasoning, input
fingerprints, and Thread-origin references are not public event fields.

Use the [events guide](../api/events.md#approve-externally-executed-tools) for
confirmation and result submission. Reconnecting clients reconcile persisted
events; the Go Session tool runner consumes the recorded outcome. A worker
restart or human confirmation does not re-evaluate an already recorded call.

## Judgment and failure behavior

Original Agent instructions and authenticated client task text supply intent.
Tool results, File and Skill text, rubric content, Web results, model summaries,
and messages from other Threads are data, and cannot grant authority. A child
Thread retains the originating client task; later queued user messages do not
expand an earlier delegation's authorization.

The first evaluator allows a clear, task-scoped invocation, asks when its
purpose, scope, destination, or effects are uncertain, and denies dangerous
secret disclosure, isolation bypass, or harmful deviations introduced by
untrusted content. A consequential action is not automatically rejected merely
because it changes state: explicit legitimate user intent can authorize it.
These are Mango criteria, with independently maintained evaluation cases, not
an implementation or quality claim about a vendor's unpublished evaluator.

Each uncached evaluation adds a model request, with a 20-second deadline and at
most 1,024 output tokens. Mango limits the complete evaluation envelope to
64 KiB. Missing or incomplete original intent, oversized input, provider
failure, refusal, or malformed output produces `ask`; an explicit interrupt
cancels the turn. Permission calls share the Session's usage and budget.

Mango saves a judgment and its known usage atomically before execution.
Execution separately validates current turn ownership and interrupts. Late
known responses can retain accounting without reviving a cancelled turn.
Retries before a receipt commits can repeat inference; provider billing is not
exactly once. Automatic evaluation is not a sandbox or a guaranteed human
checkpoint; use the self-hosted sandbox boundary and qualify the chosen model
for your workload before relying on automatic allows.


To qualify a configured model against the independently authored first-version
cases, run:

```bash
scripts/with-dev-env make test-tool-permissions-live
```

This opt-in check tests ordinary reads and edits, explicit deletion, MCP writes,
normal authentication, synthetic tutorial tokens, authorized data transfer,
ambiguous scope, secret disclosure, isolation bypass, and untrusted File, Skill,
MCP, and Thread instructions. It makes model calls but executes none of the
proposed tools. It is not required in public CI. A complete valid judgment is
required for each case; transport or parser fallback does not qualify a model.
The small case set cannot establish safety for every workload.
