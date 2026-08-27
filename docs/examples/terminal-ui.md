---
title: Try the terminal UI demo
slug: /examples/terminal-ui
---

# Try the terminal UI demo

Mango includes a reference terminal UI that exercises the HTTP and
event-stream APIs as an interactive client. It shows durable Sessions,
streaming Agent output, child Threads, action gates, and lifecycle operations
in one place.

The UI is an executable demo rather than a supported operator console or a
stable client API. Its source lives in an isolated Go module under
[`examples/terminal-ui`](https://github.com/yanpgwang/mango/tree/main/examples/terminal-ui),
so terminal-specific dependencies do not become dependencies of the Mango
server.

![Mango terminal UI example](/img/terminal-ui-demo.gif)

## Run without a server

The built-in demo is the fastest way to explore the interface. From the Mango
repository root:

```bash
cd examples/terminal-ui
go run ./cmd/mango-tui --demo
```

The in-memory backend uses the same UI commands and state projection as the
HTTP client. It includes a coordinator, two child Agents, delegation and tool
events, and a child-owned permission gate.

## Connect to the local stack

First complete [Getting started](../getting-started.md) and leave the local
stack running. Then launch the terminal UI from `examples/terminal-ui`:

```bash
MANGO_API_KEY=sk-mango-local-development \
  go run ./cmd/mango-tui
```

The client defaults to `http://127.0.0.1:8080`. To select another control
plane or attach directly to an existing Session:

```bash
MANGO_URL=https://mango.example.com \
MANGO_API_KEY=your-key \
  go run ./cmd/mango-tui -- attach sesn_...
```

The equivalent flags are `--url` and `--api-key`. Credentials in endpoint
URLs are rejected, and API keys are not persisted by the client.

The bearer key selects the Mango Workspace for every request; the client does
not send or persist a separate Workspace ID. Restart the example with a key
for another Workspace to switch resource scopes.

The connection screen shows discovered local and remembered endpoints. Select
**Enter another endpoint…** or press `e` to enter a different URL with the
Bubble Tea input control.

## What the example demonstrates

- Gap-free attach by opening every Thread stream before loading durable event
  history.
- Reconnection of the whole Thread roster when one stream ends or a new child
  Thread appears.
- A pure event-ledger projection shared by wide and compact terminal layouts.
- Session creation, rename, archive, delete, interrupt, and action-response
  flows against one backend interface.
- An in-memory backend for product review and UI regression without running a
  Mango deployment.

See the example's
[`ARCHITECTURE.md`](https://github.com/yanpgwang/mango/blob/main/examples/terminal-ui/ARCHITECTURE.md)
for the protocol and rendering boundaries.

## Verify changes

From the repository root, run:

```bash
make terminal-ui-verify
```

This runs the example's unit tests, race tests, vet checks, and build. The same
target runs in the repository CI so API and example changes are reviewed
together.
