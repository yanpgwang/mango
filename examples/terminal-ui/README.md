# Mango terminal UI example

This directory contains a reference terminal UI for exploring Mango's HTTP and
event-stream APIs. It demonstrates Session lifecycle management, live Agent
output, multi-agent Threads, action gates, and reconnect behavior in one
interactive client.

This is an example, not a supported operator console or a stable client API.
It intentionally lives in its own Go module so its Bubble Tea dependencies do
not become dependencies of the Mango server.

![Mango terminal UI example](../../website/static/img/terminal-ui-demo.gif)

## Try the built-in demo

No Mango server is required:

```bash
go run ./cmd/mango-tui --demo
```

The in-memory backend exercises the same UI commands and projections as the
HTTP client. It includes a coordinator, child Agents, delegation events, and a
child-owned permission gate.

## Connect to Mango

Complete the repository [getting started guide](../../docs/getting-started.md)
and leave the local stack running. The UI defaults to
`http://127.0.0.1:8080`:

```bash
MANGO_API_KEY=sk-mango-local-development \
  go run ./cmd/mango-tui
```

Use another endpoint or attach directly to a Session:

```bash
MANGO_URL=https://mango.example.com \
MANGO_API_KEY=your-key \
  go run ./cmd/mango-tui -- attach sesn_...
```

Equivalent flags are `--url` and `--api-key`. The selected endpoint is saved
in `mango/connection.json` under the user configuration directory; API keys
are never written there.

## Controls

- `/` opens the Session filter; `Esc` clears it.
- `Tab` cycles through the composer, conversation, and Subagent workspace.
- `Enter` opens a selected child transcript; replies still go to the
  coordinator.
- `Space` previews a child without leaving the Subagent workspace.
- `Ctrl+P`, `Ctrl+G`, `Ctrl+S`, and `Ctrl+N` open commands, Agents, Session
  search, and Session creation.
- `Ctrl+C` exits the UI without stopping remote work.

Use `--no-motion` or `MANGO_NO_MOTION=1` for static rendering. Notifications
are disabled by default; opt in with `--notify bell` or `--notify osc777`.

## Development

Run these commands from this directory:

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o bin/mango-tui ./cmd/mango-tui
vhs demo/welcome.tape
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the protocol, projection, and UI
boundaries. The repository root [LICENSE](../../LICENSE) applies to this
example.
