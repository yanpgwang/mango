# Mango Python SDK

Typed synchronous and asynchronous clients for **every operation in Mango's
current OpenAPI**: Agents, Environments and Work, Sessions and Threads, Events,
Resources, Files, Skills, Memory, Vaults, Webhooks, Deployments, and probes.
The package is local source, not a published PyPI release. Mango's development
API can change in place; keep the SDK and server on the same checkout.

## Install locally

Python 3.11 or newer is required. From the repository root:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install ./sdk/python
```

For SDK development, use `cd sdk/python && uv sync --frozen`. This creates an
isolated `.venv`, installs the editable package, and uses the checked-in lockfile.
No hosted-agent account or credential is required.

## Start a Session

```python
import os
from mango_sdk import Mango

with Mango(
    base_url=os.environ.get("MANGO_URL", "http://localhost:8080"),
    api_key=os.environ["MANGO_API_KEY"],
) as client:
    environment = client.create_environment(body={"name": "python-example"})
    agent = client.create_agent(body={
        "name": "assistant",
        "model": os.environ["MANGO_MODEL"],
        "system": "Be concise and helpful.",
    })
    session = client.create_session(body={
        "agent": {"type": "agent", "id": agent["id"]},
        "environment_id": environment["id"],
    })
    client.send_session_events(session["id"], body={"events": [{
        "type": "user.message",
        "content": [{"type": "text", "text": "Hello!"}],
    }]})
    for event in client.iter_session_events(session["id"], order="asc"):
        print(event)
```

All methods use the OpenAPI operationId in snake_case (`createAgent` becomes
`create_agent`), with positional path identifiers and keyword-only `body` and
query parameters. Bracketed query names become Python names: `types[]` becomes
`types`, and `created_at[gte]` becomes `created_at_gte`. The wire retains the
original spelling and repeats array parameters correctly. `base_url` can include
a reverse-proxy path prefix; do not append `/v1` yourself.

Request and response dictionaries have static types in `mango_sdk.models`.
Tagged unions preserve literal discriminators; all component schemas are emitted.
Closed objects are TypedDicts, not runtime validation models. Open JSON objects,
including custom-tool `input_schema`, use dictionaries so JSON Schema keywords
such as `properties`, `required`, and `$defs` remain expressible. The server
validates their constraints. Omit a dictionary key to
leave a field unchanged; pass `None` only to send explicit JSON null on a nullable
field. `False`, `0`, `""`, and `[]` are not silently dropped.

`list_*` returns one typed page. `iter_*` fetches all pages lazily, preserving
query filters. Files use `after_id`/`before_id`; other resources use `next_page`.
Do not use an unbounded iterator if you only need one page.

## Async client and streaming

```python
import asyncio
import os
from mango_sdk import AsyncMango

async def watch(session_id: str) -> None:
    async with AsyncMango(api_key=os.environ["MANGO_API_KEY"]) as client:
        session = await client.get_session(session_id)
        print(session["status"])
        async with client.stream_session_events(
            session_id, event_deltas=["agent.message"],
        ) as stream:
            async for envelope in stream:
                print(envelope.event, envelope.data)
                if envelope.event in ("session.status_idle", "session.deleted"):
                    break

# asyncio.run(watch("sesn_..."))
```

Synchronous streaming uses `with client.stream_session_events(id) as stream`
and `for envelope in stream`. Always use the context manager, especially when
breaking early. Downloads use the same lifecycle and `iter_bytes()` (an async
iterator on the async client); `read()` explicitly buffers the complete payload.
The async streaming factory is deliberately **not awaited**; the context opens it.

Streams default to no read timeout and have no finite overall deadline. HTTP
connection/write/pool timeouts still apply. A custom `httpx.Timeout` can override
these through `stream_timeout`. Closing the stream or cancelling its consuming
async task releases the connection. Streams parse split UTF-8, comments,
multiline data, and CR/LF delimiters incrementally.
Unterminated SSE lines and complete SSE frame data are each limited to 64 MiB;
oversized input raises `ResponseDecodeError` and closes the stream. HTTP error
bodies are read only up to 64 KiB (`APIError.body_truncated` indicates truncation).

**Streams are live-only.** They do not replay prior events, do not support
Last-Event-ID, and the SDK does not silently reconnect. For gap-free application
recovery, open a stream first, list history while it remains open, then merge and
deduplicate persisted events by ID. Preview frames are ephemeral and must not be
treated as authoritative history. This SDK exposes those primitives but does not
yet supply an automatic history-and-live merger.

## Uploads, downloads, and errors

```python
from mango_sdk import APIError, Mango, Upload

with Mango(api_key="workspace-key") as client:
    with open("analysis.csv", "rb") as source:
        uploaded = client.upload_file(body={
            "file": Upload("analysis.csv", source, "text/csv"),
        })
    skill = client.create_skill(body={"files": [
        Upload("analysis/SKILL.md", b"---\nname: analysis\ndescription: Analyze data\n---\n"),
    ]})
    # Only downloadable Session-scoped Files can be downloaded; client uploads cannot.
    for output in client.iter_files(scope_id="sesn_..."):
        with client.download_file(output["id"]) as stream:
            for chunk in stream.iter_bytes():
                consume(chunk)  # Your application owns the destination.
    try:
        client.get_session("sesn_missing")
    except APIError as error:
        print(error.status_code, error.type, error.request_id)
```

The caller owns file handles. Multipart uploads stream file contents through
HTTPX; async multipart file reads use ordinary blocking file handles. For unusually
slow sources, prepare local files outside the event loop first.

No HTTP request is automatically retried. An interrupted mutation may already
have committed. For example, correlate a tool-result action against persisted
events before resubmitting. Redirects are never followed, protecting bearer
credentials and avoiding mutation replay. Network failures remain normal HTTPX
exceptions; HTTP failures use `APIError`, while invalid JSON/SSE and broken cursor
progression use `ResponseDecodeError` and `PaginationError` respectively.

## Development checks

```sh
uv sync --frozen
uv run python generate.py --check
uv run pytest
uv run mypy
uv run ruff check src tests generate.py examples
```

`generate.py` consumes `../openapi.json` and `../operations.json`. Regenerate the
shared snapshot first with `go run ./scripts/sdk-contract` from the repository
root, then run `uv run python generate.py`. Generated bindings are reproducible;
the handwritten HTTP transport is not generated from any vendor implementation.
Tests are offline and deterministic. The optional `examples/conformance.py`
targets Mango's own local HTTP-handler harness, not a live model or CMA service.
