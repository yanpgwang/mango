"""Transport-level values shared by the generated public API."""

from __future__ import annotations

from dataclasses import dataclass
from typing import BinaryIO


class NotGiven:
    """The argument was omitted. Unlike ``None``, it is never sent as JSON null."""

    __slots__ = ()

    def __repr__(self) -> str:
        return "NOT_GIVEN"


NOT_GIVEN = NotGiven()


@dataclass(frozen=True)
class Upload:
    """One multipart file. The caller owns and closes an optional file handle.

    For Skills, ``filename`` is the relative bundle path, e.g. ``analysis/SKILL.md``.
    Async uploads accept ordinary binary files too; reading them is synchronous.
    """

    filename: str
    content: bytes | BinaryIO
    content_type: str = "application/octet-stream"
