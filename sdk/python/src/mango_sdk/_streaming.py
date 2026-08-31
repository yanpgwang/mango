"""Incremental SSE and binary readers; callers explicitly own stream lifetime."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator, Iterator
from dataclasses import dataclass
from types import TracebackType
from typing import TYPE_CHECKING, cast

import httpx

from ._errors import ResponseDecodeError, read_api_error, read_async_api_error

if TYPE_CHECKING:
    from .models import EventStreamFrame

MAX_SSE_FRAME_BYTES = 64 * 1024 * 1024


class _SSELines:
    """Bounded byte-level line splitting, including CR/LF across network chunks.

    HTTPX's text line iterator buffers unterminated lines without a size bound.
    Decode only complete lines here so split multibyte UTF-8 stays intact.
    """

    def __init__(self) -> None:
        self.buffer = bytearray()
        self.scan_from = 0

    def feed(self, chunk: bytes) -> Iterator[str]:
        self.buffer.extend(chunk)
        start = 0
        scan = self.scan_from
        while True:
            cr, lf = self.buffer.find(b"\r", scan), self.buffer.find(b"\n", scan)
            candidates = [index for index in (cr, lf) if index >= 0]
            if not candidates:
                self.scan_from = len(self.buffer)
                break
            index = min(candidates)
            if index - start > MAX_SSE_FRAME_BYTES:
                raise ResponseDecodeError("SSE line exceeds the 64 MiB safety limit")
            if self.buffer[index] == 13 and index == len(self.buffer) - 1:
                self.scan_from = index
                break
            end = index + 1
            if self.buffer[index] == 13 and self.buffer[index + 1] == 10:
                end += 1
            yield self.buffer[start:index].decode("utf-8", errors="replace")
            start = end
            scan = end
        if start:
            del self.buffer[:start]
            self.scan_from -= start
        if len(self.buffer) > MAX_SSE_FRAME_BYTES:
            raise ResponseDecodeError("SSE line exceeds the 64 MiB safety limit")

    def finish(self) -> Iterator[str]:
        # A final CR is itself a delimiter. Other unterminated lines are discarded.
        if self.buffer.endswith(b"\r"):
            yield from self.feed(b"\n")


@dataclass(frozen=True)
class ServerSentEvent:
    """One SSE envelope. ``data`` contains Mango's typed JSON frame.

    SSE ``id``/``retry`` are preserved if present, but never trigger automatic
    reconnection. Mango streams are live-only and do not use Last-Event-ID.
    """

    event: str
    data: EventStreamFrame
    id: str | None = None
    retry: int | None = None


class _SSEDecoder:
    def __init__(self) -> None:
        self.data: list[str] = []
        self.event = "message"
        self.id: str | None = None
        self.retry: int | None = None
        self.first = True
        self.data_size = 0

    def feed(self, line: str) -> ServerSentEvent | None:
        if self.first:
            line = line.removeprefix("\ufeff")
            self.first = False
        if line == "":
            if not self.data:
                self.event = "message"
                self.retry = None
                return None
            raw = "\n".join(self.data)
            self.data = []
            self.data_size = 0
            event, self.event = self.event, "message"
            retry, self.retry = self.retry, None
            try:
                value = json.loads(raw)
            except ValueError as error:
                raise ResponseDecodeError("SSE data is not valid JSON") from error
            if not isinstance(value, dict):
                raise ResponseDecodeError("SSE data must be a JSON object")
            return ServerSentEvent(event, cast("EventStreamFrame", value), self.id, retry)
        if line.startswith(":"):
            return None
        field, _, value = line.partition(":")
        if value.startswith(" "):
            value = value[1:]
        if field == "data":
            self.data_size += len(value.encode("utf-8")) + 1
            if self.data_size > MAX_SSE_FRAME_BYTES:
                raise ResponseDecodeError("SSE frame exceeds the 64 MiB safety limit")
            self.data.append(value)
        elif field == "event":
            self.event = value
        elif field == "id" and "\x00" not in value:
            self.id = value
        elif field == "retry" and value.isascii() and value.isdecimal():
            self.retry = int(value)
        return None


class BinaryStream:
    """Streaming binary response. Use ``with`` even when stopping iteration early."""

    def __init__(self, client: httpx.Client, request: httpx.Request) -> None:
        self._client = client
        self._request = request
        self._response: httpx.Response | None = None
        self._closed = False

    def __enter__(self) -> BinaryStream:
        self._open()
        return self

    def __exit__(
        self, exc_type: type[BaseException] | None,
        exc_value: BaseException | None, traceback: TracebackType | None,
    ) -> None:
        self.close()

    def _open(self) -> httpx.Response:
        if self._closed:
            raise RuntimeError("Stream is closed")
        if self._response is None:
            self._response = self._client.send(self._request, stream=True, follow_redirects=False)
            if not self._response.is_success:
                try:
                    raise read_api_error(self._response)
                finally:
                    self.close()
        return self._response

    @property
    def response(self) -> httpx.Response:
        return self._response if self._response is not None else self._open()

    def iter_bytes(self, chunk_size: int | None = None) -> Iterator[bytes]:
        try:
            yield from self._open().iter_bytes(chunk_size)
        finally:
            self.close()

    def read(self) -> bytes:
        try:
            return self._open().read()
        finally:
            self.close()

    def close(self) -> None:
        self._closed = True
        if self._response is not None:
            self._response.close()


class SSEStream(BinaryStream):
    def __iter__(self) -> Iterator[ServerSentEvent]:
        decoder = _SSEDecoder()
        lines = _SSELines()
        try:
            response = self._open()
            if response.headers.get("content-type", "").split(";", 1)[0] != "text/event-stream":
                raise ResponseDecodeError("Expected text/event-stream response")
            # Do not pass chunk_size: HTTPX would wait for that many bytes before
            # exposing a short frame, defeating live streaming on an idle socket.
            for chunk in response.iter_bytes():
                for line in lines.feed(chunk):
                    event = decoder.feed(line)
                    if event is not None:
                        yield event
            for line in lines.finish():
                event = decoder.feed(line)
                if event is not None:
                    yield event
            # SSE dispatch requires a blank delimiter. A truncated final event is discarded.
        finally:
            self.close()

    def __enter__(self) -> SSEStream:
        self._open()
        return self


class AsyncBinaryStream:
    """Async streaming response. An async context also closes it on cancellation."""

    def __init__(self, client: httpx.AsyncClient, request: httpx.Request) -> None:
        self._client = client
        self._request = request
        self._response: httpx.Response | None = None
        self._closed = False

    async def __aenter__(self) -> AsyncBinaryStream:
        await self._open()
        return self

    async def __aexit__(
        self, exc_type: type[BaseException] | None,
        exc_value: BaseException | None, traceback: TracebackType | None,
    ) -> None:
        await self.aclose()

    async def _open(self) -> httpx.Response:
        if self._closed:
            raise RuntimeError("Stream is closed")
        if self._response is None:
            self._response = await self._client.send(
                self._request, stream=True, follow_redirects=False,
            )
            if not self._response.is_success:
                try:
                    raise await read_async_api_error(self._response)
                finally:
                    await self.aclose()
        return self._response

    @property
    def response(self) -> httpx.Response:
        if self._response is None:
            raise RuntimeError("Enter the async stream context before reading response metadata")
        return self._response

    async def iter_bytes(self, chunk_size: int | None = None) -> AsyncIterator[bytes]:
        try:
            response = await self._open()
            async for chunk in response.aiter_bytes(chunk_size):
                yield chunk
        finally:
            await self.aclose()

    async def read(self) -> bytes:
        try:
            return await (await self._open()).aread()
        finally:
            await self.aclose()

    async def aclose(self) -> None:
        self._closed = True
        if self._response is not None:
            await self._response.aclose()


class AsyncSSEStream(AsyncBinaryStream):
    async def __aiter__(self) -> AsyncIterator[ServerSentEvent]:
        decoder = _SSEDecoder()
        lines = _SSELines()
        try:
            response = await self._open()
            if response.headers.get("content-type", "").split(";", 1)[0] != "text/event-stream":
                raise ResponseDecodeError("Expected text/event-stream response")
            async for chunk in response.aiter_bytes():
                for line in lines.feed(chunk):
                    event = decoder.feed(line)
                    if event is not None:
                        yield event
            for line in lines.finish():
                event = decoder.feed(line)
                if event is not None:
                    yield event
        finally:
            await self.aclose()

    async def __aenter__(self) -> AsyncSSEStream:
        await self._open()
        return self
