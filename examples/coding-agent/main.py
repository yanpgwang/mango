"""Repair a small Python package through Mango's public SDK, then verify its output."""

from __future__ import annotations

import argparse
import asyncio
from dataclasses import dataclass, field
from pathlib import Path
import math
import os
import sys
import tempfile
from typing import Literal

import httpx
from mango_sdk import AsyncMango, Upload
from mango_sdk.models import EventStreamFrame, SessionResourceInput

from verify import check_docker, verify_output

FIXTURES = Path(__file__).resolve().parent / "fixtures"
OUTPUT_NAME = "repaired_calc.py"  # Distinct from the Session's immutable input copies.
MAX_OUTPUT_BYTES = 64 * 1024
PROMPT = (
    "The uploaded calc.py has bugs. Copy calc.py and test_calc.py from "
    "/mnt/session/uploads into /workspace/iterate. Run python3 test_calc.py first "
    "to observe the failures, then repair calc.py and rerun the unchanged tests "
    "until all pass. Do not install packages or access the network. Publish only "
    "the repaired source at /mnt/session/outputs/repaired_calc.py."
)


@dataclass
class Turn:
    """One dedicated example Session; event IDs deduplicate replay plus live delivery."""

    seen: set[str] = field(default_factory=set)
    started: bool = False
    tool_calls: int = 0
    failed_checks: int = 0

    def observe(self, event: EventStreamFrame) -> bool:
        if event["type"] == "event_start" or event["type"] == "event_delta":
            return False
        if event["id"] in self.seen:
            return False
        self.seen.add(event["id"])
        match event["type"]:
            case "user.message":
                self.started = True
            case "agent.tool_use":
                self.tool_calls += 1
                print(f"[tool] {event['name']}", flush=True)
            case "agent.tool_result":
                failed = event.get("is_error", False)
                text = "\n".join(block["text"] for block in event.get("content", [])
                                 if block["type"] == "text")
                self.failed_checks += int(failed and any(
                    marker in text for marker in ("AssertionError", "ZeroDivisionError", "FAILED (")
                ))
                print(f"[result] {'failed — agent can revise' if failed else 'ok'}", flush=True)
            case "agent.message":
                for block in event["content"]:
                    if block["type"] == "text":
                        print(block["text"].replace("\x1b", "\\x1b"), flush=True)
            case "session.error" | "session.status_terminated" | "session.deleted":
                raise RuntimeError(f"Session needs attention: {event['type']}")
            case "session.status_idle" if self.started:
                reason = event["stop_reason"]["type"]
                if reason != "end_turn":
                    raise RuntimeError(f"Session stopped with {reason}, not end_turn")
                if not self.tool_calls or not self.failed_checks:
                    raise RuntimeError("The run did not demonstrate a failed check followed by repair")
                return True
        return False


async def observe_until_done(client: AsyncMango, session_id: str, turn: Turn) -> None:
    # Read-only recovery: open the stream first, replay all history, then merge
    # live events by ID. Never resend the task after an ambiguous HTTP response.
    while True:
        try:
            async with client.stream_session_events(session_id) as stream:
                async for event in client.iter_session_events(session_id, order="asc", limit=100):
                    if turn.observe(event):
                        return
                async for envelope in stream:
                    if turn.observe(envelope.data):
                        return
        except httpx.TransportError:
            pass
        print("Stream interrupted; reconciling persisted history...", flush=True)
        await asyncio.sleep(0.5)


@dataclass
class Created:
    session: str | None = None
    agent: str | None = None
    environment: str | None = None
    files: list[str] = field(default_factory=list)

    async def cleanup(self, client: AsyncMango) -> None:
        # Only IDs returned to this invocation. Archive retains Agent history;
        # DELETE Session releases its sandbox and removes its scoped Files.
        failures = []
        operations = [
            ("session", self.session), ("agent", self.agent),
            ("environment", self.environment), *(("file", id_) for id_ in self.files),
        ]
        for kind, id_ in operations:
            if id_ is None:
                continue
            try:
                async with asyncio.timeout(30):
                    match kind:
                        case "session":
                            await client.delete_session(id_)
                        case "agent":
                            await client.archive_agent(id_)
                        case "environment":
                            await client.delete_environment(id_)
                        case "file":
                            await client.delete_file(id_)
            except Exception:
                failures.append(f"{kind} {id_}")
        if failures:
            raise RuntimeError("Cleanup incomplete; inspect these IDs: " + ", ".join(failures))


async def create_session(client: AsyncMango, model: str, created: Created) -> str:
    # region environment
    environment = await client.create_environment(body={
        "name": "Coding-agent example", "config": {"type": "cloud"},
    })
    # endregion environment
    created.environment = environment["id"]

    # region agent
    tool_names: tuple[Literal["bash", "read", "write", "edit", "glob", "grep"], ...] = (
        "bash", "read", "write", "edit", "glob", "grep",
    )
    agent = await client.create_agent(body={
        "name": "Debugging assistant", "model": model,
        "system": "Use your tools to run failing tests, repair the code, and verify the result.",
        "tools": [{
            "type": "agent_toolset_20260401",
            "default_config": {"enabled": False},
            "configs": [{"name": name, "enabled": True,
                         "permission_policy": {"type": "always_allow"}}
                        for name in tool_names],
        }],
    })
    # endregion agent
    created.agent = agent["id"]

    # region upload
    resources: list[SessionResourceInput] = []
    for name in ("calc.py", "test_calc.py"):
        uploaded = await client.upload_file(body={
            "file": Upload(name, (FIXTURES / name).read_bytes(), "text/x-python"),
        })
        created.files.append(uploaded["id"])
        resources.append({"type": "file", "file_id": uploaded["id"],
                          "mount_path": f"/mnt/session/uploads/{name}"})
    # endregion upload

    # region session
    session = await client.create_session(body={
        "agent": {"type": "agent", "id": agent["id"], "version": agent["version"]},
        "environment_id": environment["id"], "resources": resources,
        "title": "Repair the calculator", "metadata": {"example": "coding-agent"},
    })
    # endregion session
    created.session = session["id"]
    print(f"Session: {session['id']}", flush=True)
    return session["id"]


async def download_output(client: AsyncMango, session_id: str, directory: Path) -> Path:
    # region download
    candidates = [file async for file in client.iter_files(scope_id=session_id, limit=100)
                  if file["filename"] == OUTPUT_NAME and file["downloadable"]]
    if len(candidates) != 1:
        raise RuntimeError(f"Expected exactly one published {OUTPUT_NAME}, got {len(candidates)}")
    artifact = candidates[0]
    if artifact["size_bytes"] > MAX_OUTPUT_BYTES:
        raise RuntimeError("Calculator output exceeds the example's 64 KiB limit")
    content = bytearray()
    async with client.download_file(artifact["id"]) as response:
        async for chunk in response.iter_bytes():
            content.extend(chunk)
            if len(content) > MAX_OUTPUT_BYTES:
                raise RuntimeError("Downloaded output exceeds the 64 KiB limit")
    if not content or len(content) != artifact["size_bytes"]:
        raise RuntimeError("Output is empty or its byte count does not match File metadata")
    destination = directory / OUTPUT_NAME
    with destination.open("xb") as output:
        output.write(content)
    # endregion download
    return destination


async def run(args: argparse.Namespace) -> None:
    created = Created()
    # region client
    client = AsyncMango(
        base_url=os.environ.get("MANGO_BASE_URL", "http://localhost:8080"),
        api_key=os.environ["MANGO_API_KEY"],
        timeout=30, stream_timeout=httpx.Timeout(30, read=20),
    )
    # endregion client
    async with client:
        try:
            async with asyncio.timeout(args.timeout):
                turn = Turn()
                if args.session_id:
                    session_id = args.session_id
                    session = await client.get_session(session_id)
                    if session.get("metadata", {}).get("example") != "coding-agent":
                        raise RuntimeError("Resume requires a dedicated coding-agent example Session")
                else:
                    session_id = await create_session(client, args.model, created)
                    # region stream
                    async with client.stream_session_events(session_id) as stream:
                        await client.send_session_events(session_id, body={"events": [{
                            "type": "user.message", "content": [{"type": "text", "text": PROMPT}],
                        }]})
                        try:
                            async for envelope in stream:
                                if turn.observe(envelope.data):
                                    break
                            else:
                                await observe_until_done(client, session_id, turn)
                        except httpx.TransportError:
                            await observe_until_done(client, session_id, turn)
                    # endregion stream
                if args.session_id:
                    await observe_until_done(client, session_id, turn)
                directory = Path(args.output_dir) if args.output_dir else Path(tempfile.mkdtemp(prefix="mango-iterate-"))
                directory.mkdir(parents=True, exist_ok=True)
                output = await download_output(client, session_id, directory)
                print(f"Downloaded: {output}", flush=True)
                # The downloaded module is never imported on the host.
                await asyncio.to_thread(verify_output, output, args.verifier_image)
                print("Independent verification passed. Coding-agent example completed.", flush=True)
        finally:
            if args.keep_resources:
                if created.session:
                    print(f"Kept Session: {created.session}; resume with --session-id {created.session}")
                print(f"Owned resources: {created}")
            else:
                original_error = sys.exception()
                try:
                    await created.cleanup(client)
                except Exception as error:
                    if original_error is None:
                        raise
                    print(str(error), file=sys.stderr)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", default=os.environ.get("MANGO_EXAMPLE_MODEL_ID"))
    parser.add_argument("--session-id", help="Observe/download an existing example Session; never resend or delete it")
    parser.add_argument("--keep-resources", action="store_true")
    parser.add_argument("--timeout", type=float, default=300)
    parser.add_argument("--output-dir", help="Save the verified artifact here; existing files are never overwritten")
    parser.add_argument("--verifier-image", default="python:3.12-alpine")
    args = parser.parse_args()
    if not os.environ.get("MANGO_API_KEY") or (not args.model and not args.session_id):
        parser.error("set MANGO_API_KEY and --model (or MANGO_EXAMPLE_MODEL_ID)")
    if not math.isfinite(args.timeout) or args.timeout <= 0:
        parser.error("--timeout must be finite and positive")
    check_docker(args.verifier_image)
    asyncio.run(run(args))


if __name__ == "__main__":
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        print(f"Coding-agent example failed: {str(error) or type(error).__name__}", file=sys.stderr)
        sys.exit(1)
