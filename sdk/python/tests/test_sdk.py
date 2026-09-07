from __future__ import annotations

import asyncio
import inspect
import json
import subprocess
import sys
from collections.abc import AsyncIterator, Iterator
from importlib.metadata import version
from pathlib import Path
from typing import Any, get_type_hints

import httpx
import pytest

from mango_sdk import (
    APIError,
    AsyncMango,
    Mango,
    NOT_GIVEN,
    PaginationError,
    ResponseDecodeError,
    Upload,
    __version__,
    models,
)
from mango_sdk._generated import OPERATIONS
from mango_sdk import _generated

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = json.loads((ROOT.parent / "operations.json").read_text())
OPERATION_BY_ID = {item["id"]: item for item in MANIFEST["operations"]}


def resource_method(client: Any, operation: dict[str, Any]) -> Any:
    resource = client
    for segment in operation["sdk_resource"].split("."):
        resource = getattr(resource, segment)
    return getattr(resource, operation["sdk_method"])


def test_nested_thread_stream_preserves_parent_ids_and_query_filters() -> None:
    def handle(request: httpx.Request) -> httpx.Response:
        assert request.method == "GET"
        assert request.url.raw_path == (
            b"/v1/sessions/sesn_parent/threads/sthr_child/stream?event_deltas%5B%5D=agent.message"
        )
        assert not request.content
        return httpx.Response(200, headers={"content-type": "text/event-stream"},
                              content=b'data: {"type":"agent.message","content":[]}\n\n')

    with Mango(transport=httpx.MockTransport(handle)) as client:
        with client.sessions.threads.events.stream(
            "sesn_parent", "sthr_child", event_deltas=["agent.message"],
        ) as stream:
            assert next(iter(stream)).data["type"] == "agent.message"


def test_optional_action_body_stays_absent_until_a_field_is_supplied() -> None:
    requests: list[httpx.Request] = []

    def handle(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json={})

    with Mango(transport=httpx.MockTransport(handle)) as client:
        client.deployments.run("dpl_1")
    assert requests[0].url.path == "/v1/deployments/dpl_1/run"
    assert requests[0].content == b""


def test_distribution_name_version_and_user_agent() -> None:
    assert version("mango-sdk") == __version__

    def handle(request: httpx.Request) -> httpx.Response:
        assert request.headers["User-Agent"] == f"mango-sdk-python/{__version__}"
        return httpx.Response(200, json={"id": "agent_test"})

    with Mango(transport=httpx.MockTransport(handle)) as client:
        assert client.agents.retrieve("agent_test")["id"] == "agent_test"

    async def run() -> None:
        async with AsyncMango(transport=httpx.MockTransport(handle)) as client:
            assert (await client.agents.retrieve("agent_test"))["id"] == "agent_test"

    asyncio.run(run())


class Chunks(httpx.SyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self.chunks = chunks
        self.closed = False

    def __iter__(self) -> Iterator[bytes]:
        yield from self.chunks

    def close(self) -> None:
        self.closed = True


class AsyncChunks(httpx.AsyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self.chunks = chunks
        self.closed = False

    async def __aiter__(self) -> AsyncIterator[bytes]:
        for chunk in self.chunks:
            yield chunk

    async def aclose(self) -> None:
        self.closed = True


def response_for(operation: dict[str, Any]) -> httpx.Response:
    mode = operation["mode"]
    if mode == "sse":
        return httpx.Response(200, headers={"content-type": "text/event-stream"},
                              content=b'event: session.deleted\ndata: {"type":"session.deleted"}\n\n')
    if mode == "binary":
        return httpx.Response(200, content=b"binary")
    if mode in ("text", "empty"):
        return httpx.Response(200, text="openapi: 3.1.0" if mode == "text" else "")
    return httpx.Response(200, json={})


@pytest.mark.parametrize("name", list(OPERATIONS))
def test_every_named_operation_matches_contract(name: str) -> None:
    operation = OPERATIONS[name]
    manifest = OPERATION_BY_ID[operation["id"]]
    requests: list[httpx.Request] = []

    def handle(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return response_for(operation)

    with Mango(base_url="https://mango.test/proxy", api_key="workspace-secret",
               transport=httpx.MockTransport(handle)) as client:
        kwargs = {}
        expected_path = manifest["path"]
        for parameter in manifest.get("parameters") or []:
            if parameter["in"] == "path":
                kwargs[parameter["name"]] = "identifier"
                expected_path = expected_path.replace("{" + parameter["name"] + "}", "identifier")
        if manifest["request_schema"] is not None:
            kwargs.update({"file": Upload("hello.txt", b"hello")} if name == "upload_file" else (
                {"files": [Upload("skill/SKILL.md", b"skill")]} if operation["request"] == "multipart" else {}
            ))
        method = resource_method(client, manifest)
        for parameter in inspect.signature(method).parameters.values():
            if parameter.default is inspect.Parameter.empty and parameter.name not in kwargs:
                kwargs[parameter.name] = {}
        result = method(**kwargs)
        if operation["mode"] == "sse":
            with result:
                assert len(list(result)) == 1
        elif operation["mode"] == "binary":
            with result:
                assert result.read() == b"binary"
        assert len(requests) == 1
        assert requests[0].method == manifest["method"]
        assert requests[0].url.path == "/proxy" + expected_path
        assert requests[0].headers.get("authorization") == (
            None if manifest["public"] else "Bearer workspace-secret"
        )


def test_generated_contract_and_all_component_types() -> None:
    assert len(OPERATIONS) == len(MANIFEST["operations"])
    spec = json.loads((ROOT.parent / "openapi.json").read_text())
    for name in spec["components"]["schemas"]:
        assert name in models.__all__, name
        value = getattr(models, name)
        if isinstance(value, type) and hasattr(value, "__annotations__"):
            get_type_hints(value, include_extras=True)
    subprocess.run([sys.executable, "generate.py", "--check"], cwd=ROOT, check=True)


def test_resource_method_type_hints_resolve_nested_model_aliases() -> None:
    # Signature consumers such as tool wrappers must resolve flattened union
    # arguments in both clients without supplying a custom models namespace.
    for _, resource in inspect.getmembers(_generated, inspect.isclass):
        if resource.__module__ != _generated.__name__:
            continue
        for _, method in inspect.getmembers(resource, inspect.isfunction):
            get_type_hints(method, include_extras=True)

    for client_type in (Mango, AsyncMango):
        # Inspect unbound resources to avoid opening even a mock transport.
        resource_type = get_type_hints(client_type.agents.func)["return"]
        hints = get_type_hints(resource_type.create)
        assert "model" in hints
        resource_type = get_type_hints(client_type.sessions.func)["return"]
        assert "agent" in get_type_hints(resource_type.create)


def test_path_query_and_json_omission_are_lossless() -> None:
    requests = []

    def handle(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json={})

    with Mango(base_url="https://example.test/prefix/", transport=httpx.MockTransport(handle)) as client:
        client.agents.retrieve("space/slash?#雪")
        client.sessions.events.list("id", types=["agent.message", "session.status_idle"],
                                   created_at_gte="2026-08-31T00:00:00Z", limit=0)
        client.agents.update("id", description=None, tools=[], metadata={})
        client.agents.update("id", name="", system=NOT_GIVEN)
        client.agents.list(include_archived=False)
        client.agents.retrieve("..")
    assert requests[0].url.raw_path == b"/prefix/v1/agents/space%2Fslash%3F%23%E9%9B%AA"
    assert requests[1].url.params.get_list("types[]") == ["agent.message", "session.status_idle"]
    assert requests[1].url.params["created_at[gte]"] == "2026-08-31T00:00:00Z"
    assert requests[1].url.params["limit"] == "0"
    assert json.loads(requests[2].content) == {"description": None, "tools": [], "metadata": {}}
    assert json.loads(requests[3].content) == {"name": ""}
    assert requests[4].url.params["include_archived"] == "false"
    assert requests[5].url.raw_path.endswith(b"/%2E%2E")


def test_request_errors_no_retries_or_redirect_auth_leak() -> None:
    requests = []

    def handle(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(409, json={
            "type": "error", "request_id": "req_body", "error": {
                "type": "conflict_error", "message": "Already admitted",
            },
        }, headers={"request-id": "req_header"})

    with Mango(api_key="secret", transport=httpx.MockTransport(handle)) as client:
        with pytest.raises(APIError) as caught:
            client.sessions.events.send("s", events=[])
    assert caught.value.status_code == 409
    assert caught.value.type == "conflict_error"
    assert caught.value.request_id == "req_body"
    assert "Already admitted" in str(caught.value)
    assert len(requests) == 1

    def redirect(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(307, headers={"location": "https://not-mango.test/steal"})

    with Mango(api_key="secret", transport=httpx.MockTransport(redirect)) as client:
        with pytest.raises(APIError) as caught:
            client.agents.create(name="test", model="test")
    assert caught.value.status_code == 307
    assert len(requests) == 2


def test_custom_tool_schema_keeps_arbitrary_json_schema_keywords() -> None:
    schema = {
        "type": "object", "properties": {"query": {"type": "string"}},
        "required": ["query"], "additionalProperties": False,
        "$defs": {"identifier": {"type": "string"}},
    }

    def handle(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        assert body["tools"][0]["input_schema"] == schema
        return httpx.Response(200, json={})

    with Mango(transport=httpx.MockTransport(handle)) as client:
        client.agents.create(name="lookup", model="test", tools=[{
                "type": "custom", "name": "lookup", "description": "Look up a record",
                "input_schema": schema,
            }])


def test_invalid_success_json_and_plain_error() -> None:
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(200, text="bad"))) as client:
        with pytest.raises(ResponseDecodeError):
            client.agents.retrieve("id")
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(
        502, text="upstream unavailable", headers={"request-id": "proxy-request"},
    ))) as client:
        with pytest.raises(APIError) as caught:
            client.agents.retrieve("id")
        assert caught.value.request_id == "proxy-request"
        assert caught.value.type is None


def test_error_body_is_bounded_and_closed() -> None:
    class LargeError(Chunks):
        count = 0

        def __iter__(self) -> Iterator[bytes]:
            for _ in range(1000):
                self.count += 1
                yield b"x" * 8192

    for stream_operation in (False, True):
        body = LargeError([])
        with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(502, stream=body))) as client:
            with pytest.raises(APIError) as caught:
                if stream_operation:
                    with client.files.download("id"):
                        pass
                else:
                    client.agents.retrieve("id")
            assert caught.value.body_truncated
            assert body.closed
            assert body.count <= 9


def test_multipart_files_repeated_parts_and_binary_stream() -> None:
    requests = []
    chunks = Chunks([b"hello", b" world"])

    def handle(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.url.path.endswith("/content"):
            return httpx.Response(200, stream=chunks, headers={"content-type": "application/binary"})
        return httpx.Response(200, json={})

    with Mango(transport=httpx.MockTransport(handle)) as client:
        client.files.upload(file=Upload("test.csv", b"a,b\n", "text/csv"))
        client.skills.create(display_title="Analysis", files=[
            Upload("analysis/SKILL.md", b"# skill"), Upload("analysis/lib.py", b"pass"),
        ])
        with client.files.download("file_id") as download:
            assert download.response.status_code == 200
            assert list(download.iter_bytes()) == [b"hello", b" world"]
    assert requests[0].headers["content-type"].startswith("multipart/form-data; boundary=")
    assert b'name="file"; filename="test.csv"' in requests[0].content
    assert b"Content-Type: text/csv" in requests[0].content
    assert requests[1].content.count(b'name="files"') == 2
    assert b'filename="analysis/SKILL.md"' in requests[1].content
    assert b'\r\n\r\nAnalysis\r\n' in requests[1].content
    assert chunks.closed


def test_stream_error_does_not_read_past_exact_diagnostic_bound() -> None:
    class ErrorAtLimit(Chunks):
        def __iter__(self) -> Iterator[bytes]:
            yield b"x" * (64 * 1024)
            raise AssertionError("Read past the limit instead of closing the error response")

    body = ErrorAtLimit([])
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(502, stream=body))) as client:
        with pytest.raises(APIError) as caught:
            with client.files.download("id"):
                pass
        assert caught.value.body_truncated
        assert body.closed

    async def run() -> None:
        class AsyncErrorAtLimit(AsyncChunks):
            async def __aiter__(self) -> AsyncIterator[bytes]:
                yield b"x" * (64 * 1024)
                raise AssertionError("Read past the async error limit")

        body = AsyncErrorAtLimit([])
        async with AsyncMango(transport=httpx.MockTransport(
            lambda _: httpx.Response(502, stream=body),
        )) as client:
            with pytest.raises(APIError) as caught:
                async with client.sessions.events.stream("id"):
                    pass
            assert caught.value.body_truncated
            assert body.closed

    asyncio.run(run())


def test_sse_incremental_utf8_comments_multiline_and_truncated_eof() -> None:
    raw = ('\ufeff: keepalive\r\nevent: agent.message\r\nid: event-1\r\nretry: 2500\r\n'
           'data: {"type":"agent.message",\r\ndata: "content":[{"type":"text","text":"雪"}]}\r\n\r\n'
           'event: session.deleted\rdata: {"type":"session.deleted"}\r\r'
           'event: incomplete\ndata: {"type":"unfinished"}').encode()
    chunks = Chunks([raw[index:index + 1] for index in range(len(raw))])
    requests = []

    def handle(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, stream=chunks, headers={"content-type": "text/event-stream; charset=utf-8"})

    with Mango(transport=httpx.MockTransport(handle), timeout=0.1) as client:
        with client.sessions.events.stream("s", event_deltas=["agent.message"]) as stream:
            events = list(stream)
    assert len(events) == 2
    assert events[0].event == "agent.message"
    assert events[0].data["content"][0]["text"] == "雪"
    assert events[0].id == "event-1"
    assert events[0].retry == 2500
    assert events[1].event == "session.deleted"
    assert chunks.closed
    assert "last-event-id" not in requests[0].headers
    assert requests[0].extensions["timeout"]["read"] is None


def test_stream_closes_on_early_exit_and_invalid_sse() -> None:
    chunks = Chunks([b'data: {"type":"session.deleted"}\n\n', b'data: {}\n\n'])
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(
        200, stream=chunks, headers={"content-type": "text/event-stream"},
    ))) as client:
        with client.sessions.events.stream("s") as stream:
            for _ in stream:
                break
        assert chunks.closed
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(
        200, content=b"data: {broken}\n\n", headers={"content-type": "text/event-stream"},
    ))) as client:
        with pytest.raises(ResponseDecodeError), client.sessions.events.stream("s") as stream:
            list(stream)


def test_sse_small_frame_is_delivered_before_reading_another_chunk() -> None:
    class LiveStream(Chunks):
        def __iter__(self) -> Iterator[bytes]:
            yield b'data: {"type":"session.deleted"}\n\n'
            raise AssertionError("The client read ahead instead of yielding the live frame")

    body = LiveStream([])
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(
        200, stream=body, headers={"content-type": "text/event-stream"},
    ))) as client:
        with client.sessions.events.stream("s") as stream:
            event = next(iter(stream))
            assert event.data["type"] == "session.deleted"
    assert body.closed


@pytest.mark.parametrize("payload", [b"data: " + b"x" * 33, b"data: " + b"x" * 20 + b"\n" + b"data: " + b"x" * 20 + b"\n"])
def test_sse_limits_close_sync_and_async_streams(monkeypatch: pytest.MonkeyPatch, payload: bytes) -> None:
    monkeypatch.setattr("mango_sdk._streaming.MAX_SSE_FRAME_BYTES", 32)
    body = Chunks([payload])
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(
        200, stream=body, headers={"content-type": "text/event-stream"},
    ))) as client:
        with pytest.raises(ResponseDecodeError, match="safety limit"):
            with client.sessions.events.stream("s") as stream:
                list(stream)
    assert body.closed

    async def run() -> None:
        async_body = AsyncChunks([payload])
        async with AsyncMango(transport=httpx.MockTransport(lambda _: httpx.Response(
            200, stream=async_body, headers={"content-type": "text/event-stream"},
        ))) as client:
            with pytest.raises(ResponseDecodeError, match="safety limit"):
                async with client.sessions.events.stream("s") as stream:
                    [event async for event in stream]
        assert async_body.closed

    asyncio.run(run())


def test_cursor_and_files_pagination_keep_filters_and_direction() -> None:
    requests = []

    def handle(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.url.path.endswith("/files"):
            cursor = request.url.params.get("after_id") or request.url.params.get("before_id")
            return httpx.Response(200, json={
                "data": [{"id": cursor or "first"}], "has_more": cursor in (None, "start"),
                "first_id": "boundary", "last_id": "boundary",
            })
        return httpx.Response(200, json={
            "data": [{"id": request.url.params.get("page") or "one"}],
            "next_page": None if "page" in request.url.params else "two",
        })

    with Mango(transport=httpx.MockTransport(handle)) as client:
        assert [item["id"] for item in client.agents.iter(limit=1, include_archived=False)] == ["one", "two"]
        assert [item["id"] for item in client.files.iter(scope_id="s")] == ["first", "boundary"]
        assert [item["id"] for item in client.files.iter(before_id="start")] == ["start", "boundary"]
    assert requests[1].url.params["limit"] == "1"
    assert requests[1].url.params["include_archived"] == "false"
    assert requests[3].url.params["scope_id"] == "s"
    assert requests[3].url.params["after_id"] == "boundary"
    assert requests[5].url.params["before_id"] == "boundary"
    assert "after_id" not in requests[5].url.params


def test_pagination_rejects_repeated_and_missing_cursors() -> None:
    for page in (
        {"data": [], "next_page": "same"},
        {"data": [], "has_more": True, "last_id": None},
    ):
        with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(200, json=page))) as client:
            with pytest.raises(PaginationError):
                list(client.files.iter() if "has_more" in page else client.agents.iter())


def test_pagination_detects_multi_page_cycles() -> None:
    cursors = iter(["a", "b", "a"])
    with Mango(transport=httpx.MockTransport(lambda _: httpx.Response(
        200, json={"data": [], "next_page": next(cursors)},
    ))) as client:
        with pytest.raises(PaginationError, match="cycled"):
            list(client.agents.iter())


def test_async_all_named_operations_and_pagination() -> None:
    async def run() -> None:
        for name, operation in OPERATIONS.items():
            requests = []

            async def handle(request: httpx.Request) -> httpx.Response:
                requests.append(request)
                return response_for(operation)

            async with AsyncMango(transport=httpx.MockTransport(handle)) as client:
                method = resource_method(client, OPERATION_BY_ID[operation["id"]])
                kwargs = {}
                for parameter in inspect.signature(method).parameters.values():
                    if parameter.default is inspect.Parameter.empty:
                        kwargs[parameter.name] = {} if parameter.name == "body" else "id"
                if operation["request"] == "multipart":
                    kwargs.update({"file": Upload("file", b"data")} if name == "upload_file" else {"files": [Upload("file", b"data")]})
                result = method(**kwargs)
                if operation["mode"] in ("sse", "binary"):
                    async with result:
                        if operation["mode"] == "sse":
                            assert len([event async for event in result]) == 1
                        else:
                            assert await result.read() == b"binary"
                else:
                    await result
                assert len(requests) == 1
                assert requests[0].method == operation["method"]

        async def pages(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, json={"data": [{"id": "a"}], "next_page": None})

        async with AsyncMango(transport=httpx.MockTransport(pages)) as client:
            assert [item["id"] async for item in client.agents.iter()] == ["a"]

    asyncio.run(run())


def test_async_cancellation_closes_network_stream() -> None:
    async def run() -> None:
        reading = asyncio.Event()

        class WaitingStream(httpx.AsyncByteStream):
            closed = False

            async def __aiter__(self) -> AsyncIterator[bytes]:
                yield b": waiting\n"
                reading.set()
                await asyncio.Event().wait()

            async def aclose(self) -> None:
                self.closed = True

        body = WaitingStream()

        async def handle(_: httpx.Request) -> httpx.Response:
            return httpx.Response(200, stream=body, headers={"content-type": "text/event-stream"})

        async with AsyncMango(transport=httpx.MockTransport(handle)) as client:
            async def consume() -> None:
                async with client.sessions.events.stream("s") as stream:
                    async for _ in stream:
                        pass

            task = asyncio.create_task(consume())
            await asyncio.wait_for(reading.wait(), timeout=1)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
        assert body.closed

    asyncio.run(run())


@pytest.mark.parametrize("url", ["ftp://example.test", "https://user:secret@example.test", "https://example.test?key=x", "relative"])
def test_invalid_base_url(url: str) -> None:
    with pytest.raises(ValueError):
        Mango(base_url=url)
