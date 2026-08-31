"""Mango errors preserve the HTTP status and server correlation identifier."""

from __future__ import annotations

import json
from typing import Any

import httpx

MAX_ERROR_BODY_BYTES = 64 * 1024


class MangoError(Exception):
    """Base class for SDK protocol and HTTP errors (not transport failures)."""


class APIError(MangoError):
    def __init__(
        self, response: httpx.Response, *, body: bytes | None = None, truncated: bool = False,
    ) -> None:
        self.status_code = response.status_code
        self.response = response
        self.request_id = response.headers.get("request-id")
        self.type: str | None = None
        self.body: Any = None
        raw = response.content if body is None else body
        self.body_truncated = truncated or len(raw) > MAX_ERROR_BODY_BYTES
        raw = raw[:MAX_ERROR_BODY_BYTES]
        try:
            self.body = json.loads(raw)
        except ValueError:
            pass
        message = raw.decode("utf-8", errors="replace")[:4096] or response.reason_phrase
        if isinstance(self.body, dict):
            self.request_id = self.body.get("request_id") or self.request_id
            error = self.body.get("error")
            if isinstance(error, dict):
                self.type = error.get("type")
                message = error.get("message") or message
        if self.body_truncated:
            message += " [error body truncated]"
        super().__init__(f"HTTP {self.status_code}: {message}")


def read_api_error(response: httpx.Response) -> APIError:
    """Bound diagnostics even if a reverse proxy returns a huge response body."""
    data = bytearray()
    try:
        for chunk in response.iter_bytes():
            data.extend(chunk[:MAX_ERROR_BODY_BYTES - len(data)])
            if len(data) >= MAX_ERROR_BODY_BYTES:
                break
        # Do not wait for one extra byte to prove truncation: streaming routes
        # have no read deadline and an error response can stall at the bound.
        return APIError(response, body=bytes(data), truncated=len(data) >= MAX_ERROR_BODY_BYTES)
    finally:
        response.close()


async def read_async_api_error(response: httpx.Response) -> APIError:
    data = bytearray()
    try:
        async for chunk in response.aiter_bytes():
            data.extend(chunk[:MAX_ERROR_BODY_BYTES - len(data)])
            if len(data) >= MAX_ERROR_BODY_BYTES:
                break
        return APIError(response, body=bytes(data), truncated=len(data) >= MAX_ERROR_BODY_BYTES)
    finally:
        await response.aclose()


class ResponseDecodeError(MangoError):
    """A successful response did not contain the documented JSON payload."""


class PaginationError(MangoError):
    """A server pagination response would otherwise cause an infinite loop."""
