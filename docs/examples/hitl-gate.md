---
title: Human-in-the-loop gate
description: Review expenses with the Go SDK and resume the same Session after a client restart.
slug: /examples/hitl-gate
---

# Human-in-the-loop gate

Build an application that records a clear expense decision and asks a human
about an ambiguous one. Mango waits for both custom-tool results before the
Agent continues. Stop the client at the gate, restart it, and finish the same
Session without repeating the recorded decision.

The [Go application source](https://github.com/yanpgwang/mango/tree/main/examples/hitl-gate)
uses Mango's public SDK. Its private local state file is the **simulated expense
system**: saving a decision is the only business effect. It does not submit
payments or connect to a real expense service.

## Prerequisites

- Complete the [Quickstart](../getting-started.md), then configure a real model
  on the Mango worker using the [model configuration guide](../guides/model-configuration.md).
- Keep that Mango deployment running. The model must support custom tools;
  this example does not need a sandbox tool call.
- Run from a Mango repository checkout with **Go 1.26.6 or newer**. This root
  application uses the source Go SDK through the repository's local module
  replacement. The standalone SDK quickstart has its own Go 1.24 minimum.
- Use one terminal process and one operator per state file.

```sh
export MANGO_EXAMPLE_BASE_URL=http://localhost:8080
export MANGO_API_KEY=sk-mango-local-development
export MANGO_EXAMPLE_MODEL_ID=your-configured-model-id
```

Use your deployment's Workspace key in place of the local development key.
Only `start` needs the model ID. Model-provider credentials belong on the Mango
worker; the client reads no model credential and stores no credentials.

## Run and recognize success

```sh
go run ./examples/hitl-gate start
```

The program creates an Environment, Agent, and Session and saves their IDs to
`.mango/hitl-gate.json`. The model proposes two custom actions:

| Receipt | Input | Who decides |
| --- | --- | --- |
| `r01` | USD 12 for office pencils, itemized receipt | Application records the proposed clear decision |
| `r02` | USD 900 for an unspecified team activity, no itemized receipt | You enter `approve` or `reject` |

The default run waits for you at the second receipt. Output resembles:

```text
Created Environment env_...
Created Agent agent_...
Created Session sess_...
Session sess_...; state: .mango/hitl-gate.json
Recorded approve for r01 (application).
Result persisted for sevt_...

Human review for r02: ...
Decision [approve/reject]: reject
Recorded reject for r02 (human).
Result persisted for sevt_...

Agent final response:
...the model summarizes both recorded outcomes...
Resources retained. Use cleanup with the same -state path when finished.
```

IDs, the review question, and final prose vary with the model. A successful run
records both results and observes `end_turn` with a final Agent message. The
example rejects a model response that does not contain the two expected actions.

`make demo-hitl-gate` runs `start` as well. Pass another command with, for
example, `make demo-hitl-gate HITL_ARGS=resume`.

## Read the SDK flow

The client connects to your Mango server with a Workspace credential:

::include[../../examples/hitl-gate/main.go#client]{lang="go"}

The Agent declares custom tools. `customTool` builds each tool's JSON Schema;
the application handles the requests outside the sandbox:

::include[../../examples/hitl-gate/main.go#agent]{lang="go"}

The Session uses the created resources:

::include[../../examples/hitl-gate/main.go#session]{lang="go"}

The application polls **persisted event history**, using the SDK iterator to
read every page. A `requires_action` idle event identifies the blocking custom
actions. The application subtracts IDs already answered by persisted
`user.custom_tool_result` events before asking for or submitting anything:

::include[../../examples/hitl-gate/main.go#history]{lang="go"}

The local decision journal is keyed by the custom-tool event ID. Saving happens
before `Sessions.Events.Send`; the same ID becomes `custom_tool_use_id`:

::include[../../examples/hitl-gate/main.go#decision]{lang="go"}

This application uses explicit event operations so the human decision and local
journal remain visible. See the [Events API](../api/events.md) for the runtime's
barrier and duplicate-result semantics and the [SDK guide](../sdk.md) for its
other resources and helpers.

## Stop, resume, and clean up

Try a deterministic restart before the human decision:

```sh
go run ./examples/hitl-gate start -state .mango/restart.json -stop-after-first-result
go run ./examples/hitl-gate resume -state .mango/restart.json
```

The first command exits after submitting the clear expense result. The second
reads the existing Session history, skips that result, and asks for the human
decision. You can also press **Ctrl-C** while the normal run waits for input,
then use `resume`. Input cancellation preserves the state file and resources.

`resume` uses the saved Session ID. It never creates another Session or sends
the initial prompt again. If a result response fails after the server accepted
it, the next run finds it in history. If the result is absent, it sends the
already saved decision without asking you again. Keep the same server URL,
Workspace credential, and state file across runs; a different server URL is
rejected before any resume or cleanup request.

Every state update writes a complete private file (mode `0600`) and atomically
replaces the previous file. New state directories use `0700`. `start` refuses
to overwrite any existing state. The default `.mango/` directory is Git-ignored;
keep custom state paths out of source control as well.

Delete the Session, archive the Agent, and delete the Environment explicitly:

```sh
go run ./examples/hitl-gate cleanup -state .mango/restart.json
# For the default run:
go run ./examples/hitl-gate cleanup
```

Cleanup saves progress after each successful operation. On failure it keeps
the remaining IDs and reports the failed resource; retry the same command.
It removes the state file only after known cleanup is complete. Once cleanup
starts, the state cannot be resumed.

If initial setup fails, already returned IDs stay in the state file. A lost
resource-creation response may leave an unknown resource: the state records the
creation stage, and cleanup retains it with instructions to inspect the
corresponding [Environment](../api/environments.md), [Agent](../api/agents.md),
or [Session](../api/sessions.md) list for the printed ID, `HITL` name, or
`Interactive expense review` title. Clean up the uncertain resource manually
before removing that state file. If no initial user message reached the
Session, `resume` reports that condition for inspection and cleanup.

## Limits

This is one tutorial Session owned by one application and one operator. Do not
run two processes on the same state file, switch Workspace keys, or send extra
messages/results from another client. The server URL check cannot detect a
changed Workspace credential or a different server installed at the same URL.

The local journal demonstrates restart-safe application decisions; it is not an
exactly-once guarantee for an external payment, email, or database write. A real
integration needs its own durable transaction or idempotency mechanism keyed by
the custom-tool event ID. Losing or manually changing the journal can lose the
record of application work.

History polling reads the complete log on each pass. That keeps the small
example understandable; large applications should maintain their own cursor
and projection. Errors preserve resources for inspection and explicit cleanup.
The adjacent package tests exercise this application's SDK calls and state-file
recovery with an independent HTTP fixture. Mango's runtime, persistence, and
service tests remain separate; fixture success does not establish a real-model
run of this tutorial.
