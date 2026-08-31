"""Shared transport; never retries mutations or forwards bearer auth on redirects."""

from __future__ import annotations

from collections.abc import AsyncIterator, Iterator, Mapping
from types import TracebackType
from typing import Any
from urllib.parse import quote, urlsplit

import httpx

from ._errors import APIError, PaginationError, ResponseDecodeError, read_api_error, read_async_api_error
from ._streaming import AsyncBinaryStream, AsyncSSEStream, BinaryStream, SSEStream
from ._types import NOT_GIVEN, NotGiven, Upload
from ._version import __version__


def _url(base_url: str, path: str, values: Mapping[str, Any]) -> str:
    for key, value in values.items():
        encoded = quote(str(value), safe="")
        if encoded in (".", ".."):
            encoded = encoded.replace(".", "%2E")
        path = path.replace("{" + key + "}", encoded)
    return base_url + path


def _clean(value: Any) -> Any:
    if isinstance(value, dict):
        return {key: _clean(item) for key, item in value.items() if not isinstance(item, NotGiven)}
    if isinstance(value, (list, tuple)):
        if any(isinstance(item, NotGiven) for item in value):
            raise TypeError("NOT_GIVEN is not a valid array element")
        return [_clean(item) for item in value]
    return value


def _scalar(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bool):
        return "true" if value else "false"
    return str(value)


def _query(values: Mapping[str, Any]) -> list[tuple[str, str]]:
    result: list[tuple[str, str]] = []
    for name, value in values.items():
        if isinstance(value, NotGiven):
            continue
        if isinstance(value, (list, tuple)):
            result.extend((name, _scalar(item)) for item in value)
        else:
            result.append((name, _scalar(value)))
    return result


def _multipart(body: Mapping[str, Any]) -> list[tuple[str, Any]]:
    parts: list[tuple[str, Any]] = []
    for name, value in body.items():
        if isinstance(value, NotGiven):
            continue
        for item in value if isinstance(value, list) else [value]:
            if isinstance(item, Upload):
                parts.append((name, (item.filename, item.content, item.content_type)))
            elif item is None:
                raise TypeError("Multipart fields cannot be null")
            else:
                parts.append((name, (None, _scalar(item))))
    return parts


def _check_base(base_url: str) -> str:
    parts = urlsplit(base_url)
    if parts.scheme not in ("http", "https") or not parts.netloc:
        raise ValueError("base_url must be an absolute http(s) URL")
    if parts.query or parts.fragment or parts.username or parts.password:
        raise ValueError("base_url cannot contain credentials, a query, or a fragment")
    return base_url.rstrip("/")


def _next_page(page: Mapping[str, Any], query: dict[str, Any]) -> bool:
    """Mutate a local query copy, preserving Mango's two different cursor families."""
    if "next_page" in page:
        cursor = page["next_page"]
        if cursor is None or cursor == "":
            return False
        key = "page"
    elif page.get("has_more"):
        key = "before_id" if query.get("before_id", NOT_GIVEN) not in (NOT_GIVEN, None, "") else "after_id"
        cursor = page.get("first_id" if key == "before_id" else "last_id")
        if not cursor:
            raise PaginationError("Files page has_more=true without a usable boundary ID")
    else:
        return False
    if cursor == query.get(key):
        raise PaginationError("Server repeated its pagination cursor")
    query[key] = cursor
    return True


def _decode(response: httpx.Response, mode: str) -> Any:
    if not response.is_success:
        raise APIError(response)
    if mode == "empty":
        return None
    if mode == "text":
        return response.text
    try:
        return response.json()
    except ValueError as error:
        raise ResponseDecodeError(
            f"Invalid JSON in HTTP {response.status_code} response "
            f"(request-id={response.headers.get('request-id')!r})"
        ) from error


class _RequestBuilder:
    _http: httpx.Client | httpx.AsyncClient
    _base_url: str
    _api_key: str | None
    _stream_timeout: httpx.Timeout

    def _build(
        self, operation: Mapping[str, Any], path: Mapping[str, Any],
        query: Mapping[str, Any], body: Any = NOT_GIVEN,
    ) -> httpx.Request:
        headers = {"Accept": operation["accept"]}
        if not operation["public"] and self._api_key:
            headers["Authorization"] = f"Bearer {self._api_key}"
        kwargs: dict[str, Any] = {
            "params": _query(query), "headers": headers,
        }
        if not isinstance(body, NotGiven):
            if operation["request"] == "multipart":
                kwargs["files"] = _multipart(body)
            else:
                # httpx treats json=None as no body. Encode null explicitly instead.
                import json
                kwargs["content"] = json.dumps(_clean(body), ensure_ascii=False).encode("utf-8")
                headers["Content-Type"] = "application/json"
        if operation["mode"] in ("sse", "binary"):
            kwargs["timeout"] = self._stream_timeout
        return self._http.build_request(
            operation["method"], _url(self._base_url, operation["path"], path), **kwargs,
        )


class BaseClient(_RequestBuilder):
    """HTTPX-backed client. Timeout is per network operation, not a total deadline.

    No requests are retried automatically. An ambiguous mutation may already be
    committed; reconcile persisted server state before deciding to resubmit it.
    ``transport`` is an explicit seam for local testing or custom networking.
    """

    _http: httpx.Client

    def __init__(
        self, *, base_url: str = "http://localhost:8080", api_key: str | None = None,
        timeout: float | httpx.Timeout = 30.0,
        stream_timeout: httpx.Timeout | None = None,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        self._base_url = _check_base(base_url)
        self._api_key = api_key
        self._stream_timeout = stream_timeout or httpx.Timeout(30.0, read=None)
        self._http = httpx.Client(
            timeout=timeout, transport=transport, follow_redirects=False,
            headers={"User-Agent": f"mango-sdk-python/{__version__}"},
        )

    def __enter__(self) -> BaseClient:
        return self

    def __exit__(
        self, exc_type: type[BaseException] | None,
        exc_value: BaseException | None, traceback: TracebackType | None,
    ) -> None:
        self.close()

    def close(self) -> None:
        self._http.close()

    def _request(
        self, operation: Mapping[str, Any], path: Mapping[str, Any],
        query: Mapping[str, Any], body: Any = NOT_GIVEN,
    ) -> Any:
        response = self._http.send(self._build(operation, path, query, body), stream=True)
        try:
            if not response.is_success:
                raise read_api_error(response)
            response.read()
            return _decode(response, operation["mode"])
        finally:
            response.close()

    def _stream(
        self, operation: Mapping[str, Any], path: Mapping[str, Any], query: Mapping[str, Any],
    ) -> SSEStream:
        return SSEStream(self._http, self._build(operation, path, query))

    def _binary(
        self, operation: Mapping[str, Any], path: Mapping[str, Any], query: Mapping[str, Any],
    ) -> BinaryStream:
        return BinaryStream(self._http, self._build(operation, path, query))

    def _paginate(
        self, operation: Mapping[str, Any], path: Mapping[str, Any], query: dict[str, Any],
    ) -> Iterator[Any]:
        seen: set[str] = set()
        while True:
            result = self._request(operation, path, query)
            yield from result["data"]
            if not _next_page(result, query):
                return
            fingerprint = repr(query)
            if fingerprint in seen:
                raise PaginationError("Server cycled through pagination cursors")
            seen.add(fingerprint)


class AsyncBaseClient(_RequestBuilder):
    """Asynchronous equivalent of BaseClient; cancellation propagates to HTTPX."""

    _http: httpx.AsyncClient

    def __init__(
        self, *, base_url: str = "http://localhost:8080", api_key: str | None = None,
        timeout: float | httpx.Timeout = 30.0,
        stream_timeout: httpx.Timeout | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        self._base_url = _check_base(base_url)
        self._api_key = api_key
        self._stream_timeout = stream_timeout or httpx.Timeout(30.0, read=None)
        self._http = httpx.AsyncClient(
            timeout=timeout, transport=transport, follow_redirects=False,
            headers={"User-Agent": f"mango-sdk-python/{__version__}"},
        )

    async def __aenter__(self) -> AsyncBaseClient:
        return self

    async def __aexit__(
        self, exc_type: type[BaseException] | None,
        exc_value: BaseException | None, traceback: TracebackType | None,
    ) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _request(
        self, operation: Mapping[str, Any], path: Mapping[str, Any],
        query: Mapping[str, Any], body: Any = NOT_GIVEN,
    ) -> Any:
        response = await self._http.send(self._build(operation, path, query, body), stream=True)
        try:
            if not response.is_success:
                raise await read_async_api_error(response)
            await response.aread()
            return _decode(response, operation["mode"])
        finally:
            await response.aclose()

    def _stream(
        self, operation: Mapping[str, Any], path: Mapping[str, Any], query: Mapping[str, Any],
    ) -> AsyncSSEStream:
        return AsyncSSEStream(self._http, self._build(operation, path, query))

    def _binary(
        self, operation: Mapping[str, Any], path: Mapping[str, Any], query: Mapping[str, Any],
    ) -> AsyncBinaryStream:
        return AsyncBinaryStream(self._http, self._build(operation, path, query))

    async def _paginate(
        self, operation: Mapping[str, Any], path: Mapping[str, Any], query: dict[str, Any],
    ) -> AsyncIterator[Any]:
        seen: set[str] = set()
        while True:
            result = await self._request(operation, path, query)
            for item in result["data"]:
                yield item
            if not _next_page(result, query):
                return
            fingerprint = repr(query)
            if fingerprint in seen:
                raise PaginationError("Server cycled through pagination cursors")
            seen.add(fingerprint)
