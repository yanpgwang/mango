---
title: Coordinate a specialist team
slug: /examples/multi-agent-team
---

# Coordinate a specialist team

This example runs a release-readiness review through Mango's public HTTP API.
The current Go client uses `net/http`; the same operations are available in
the first-party [SDKs](../sdk.md).
A coordinator delegates independent work to reliability and security Agents,
consults an Advisor, waits for all three responses, and produces a decision. A
second interactive turn adds a new constraint and must continue the existing
reliability Thread instead of creating another one.

Every inference in the documented run uses the real model endpoint configured
on the Mango worker. The example client has no model-provider credentials and
does not invoke a hosted agent service. It also uses no simulated GitHub, web,
database, or other third-party boundary: the workflow exercises only Mango's
Agents, Session, Events, and Session Threads.

## What the run proves

The program checks observable product behavior rather than matching exact model
wording:

1. The coordinator starts exactly one `reliability_reviewer` Thread and one
   `security_reviewer` Thread.
2. Both specialists return non-empty reports with real provider token usage.
3. The coordinator does not produce its decision until both specialist reports
   and the configured Advisor's challenge are available. The model may consult
   the Advisor concurrently with the specialists.
4. The Advisor appears as one automatically terminated
   `{"type":"advisor"}` Session Thread with its own usage.
5. The terminal user supplies a follow-up constraint.
6. The coordinator addresses the existing reliability Thread with
   `session_thread_id` and waits for its new report.
7. The follow-up increases usage on that same Thread without creating another
   specialist or Advisor Thread.

Mango's ordinary offline service tests cover the deterministic delegation,
Advisor, persistence, retry, interrupt, and follow-up invariants. This example
adds the explicitly opt-in real-model evidence that public CI cannot provide.

## Run the example

Configure a running API and worker using
[Use a real model endpoint](../getting-started.md#use-a-real-model-endpoint).
With the same `~/.config/mango/dev.env`, run the public-HTTP client:

```bash
scripts/with-dev-env make demo-multi-agent-team
```

The example does not start services or change the worker's model or sandbox.

The Make target passes the configured model IDs to the example but removes the
provider base URL and key from the client process. Set
`MANGO_EXAMPLE_ADVISOR_MODEL_ID` when the Advisor should use a different model;
otherwise it uses `MANGO_EXAMPLE_MODEL_ID`.

After the first decision, the terminal asks for another release constraint.
Enter one or press Return to use the displayed default. The real coordinator
must route it through the persistent reliability Thread before returning a
revised decision.

Set `MANGO_EXAMPLE_KEEP_RESOURCES=1` to retain the Session, three Agent
resources, and Environment for inspection. Otherwise the program deletes the
Session and Environment and archives the Agents after the verification.

## Design boundary

The specialist-team user problem is informed by Anthropic's public
[`CMA_coordinate_specialist_team` cookbook](https://github.com/anthropics/claude-cookbooks/blob/main/managed_agents/CMA_coordinate_specialist_team.ipynb).
Mango adopts the useful coordinator, scoped specialist, persistent Thread, and
Advisor workflow. It replaces the hosted data, web-search, SDK, and
`send_to_parent` presentation with synthetic release facts, Mango's public HTTP
API, and runtime-owned child completion reports.

The example does not define Mango's multi-agent contract and adds no
scenario-specific runtime behavior. See
[Run a multi-agent Session](../guides/multi-agent.md) for the reusable workflow
and [Session Threads](../api/session-threads.md) for the HTTP contract.
