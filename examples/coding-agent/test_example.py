"""Offline example tests. The separate Go harness exercises real services."""

import asyncio
import argparse
from contextlib import asynccontextmanager
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import AsyncMock, patch

from main import Created, Turn, download_output, observe_until_done, run
from verify import check_docker, verify_output


def event(kind, id_, **fields):
    return {"type": kind, "id": id_, **fields}


def completed_events():
    return [
        event("user.message", "message"),
        event("agent.tool_use", "call", name="bash"),
        event("agent.tool_result", "failure", is_error=True,
              content=[{"type": "text", "text": "AssertionError: initial failure"}]),
        event("session.status_idle", "idle", stop_reason={"type": "end_turn"}),
    ]


class TurnTests(unittest.TestCase):
    def test_replay_deduplicates_and_requires_actual_work(self):
        turn = Turn()
        events = completed_events()
        for item in events[:-1]:
            self.assertFalse(turn.observe(item))
            self.assertFalse(turn.observe(item))
        self.assertEqual(turn.tool_calls, 1)
        self.assertEqual(turn.failed_checks, 1)
        self.assertTrue(turn.observe(events[-1]))

    def test_success_text_without_tools_is_not_success(self):
        turn = Turn(started=True)
        turn.observe(event("agent.message", "text", content=[{"type": "text", "text": "All fixed!"}]))
        with self.assertRaisesRegex(RuntimeError, "did not demonstrate"):
            turn.observe(completed_events()[-1])

    def test_tool_failure_is_not_necessarily_a_failed_test(self):
        turn = Turn(started=True, tool_calls=1)
        turn.observe(event("agent.tool_result", "error", is_error=True,
                           content=[{"type": "text", "text": "File not found"}]))
        with self.assertRaisesRegex(RuntimeError, "did not demonstrate"):
            turn.observe(completed_events()[-1])

    def test_requires_action_error_and_termination_fail_closed(self):
        for item in [event("session.error", "error"), event("session.status_terminated", "stop"),
                     event("session.status_idle", "idle", stop_reason={"type": "requires_action"})]:
            with self.subTest(type=item["type"]), self.assertRaises(RuntimeError):
                Turn(started=True).observe(item)

    def test_preview_is_not_a_persisted_event(self):
        turn = Turn()
        self.assertFalse(turn.observe({"type": "event_delta", "event_id": "preview"}))
        self.assertFalse(turn.observe({"type": "event_start", "event": {"id": "preview"}}))
        self.assertFalse(turn.seen)


class AsyncTests(unittest.IsolatedAsyncioTestCase):
    async def test_resume_opens_stream_before_history_and_never_sends(self):
        order = []

        class Client:
            @asynccontextmanager
            async def stream_session_events(self, session_id):
                order.append("open")
                try:
                    yield None  # Complete history means the live iterator is not needed.
                finally:
                    order.append("close")

            async def iter_session_events(self, session_id, **query):
                order.append("history")
                self.assert_query = query
                for item in completed_events():
                    yield item

        await observe_until_done(Client(), "session", Turn())
        self.assertEqual(order, ["open", "history", "close"])

    async def test_history_recovery_remains_bounded_by_outer_deadline(self):
        class Client:
            @asynccontextmanager
            async def stream_session_events(self, session_id):
                await asyncio.sleep(60)
                yield None

        with self.assertRaises(TimeoutError):
            async with asyncio.timeout(0.01):
                await observe_until_done(Client(), "session", Turn())

    async def test_cleanup_continues_after_one_failure_and_only_owns_recorded_ids(self):
        client = AsyncMock()
        client.delete_session.side_effect = RuntimeError("unreachable")
        created = Created(session="s", agent="a", environment="e", files=["f"])
        with self.assertRaisesRegex(RuntimeError, "session s"):
            await created.cleanup(client)
        client.delete_session.assert_awaited_once_with("s")
        client.archive_agent.assert_awaited_once_with("a")
        client.delete_environment.assert_awaited_once_with("e")
        client.delete_file.assert_awaited_once_with("f")
        empty_client = AsyncMock()
        await Created().cleanup(empty_client)
        self.assertEqual(empty_client.mock_calls, [])

    async def test_output_must_be_unambiguous_and_not_overwrite_local_files(self):
        class Client:
            def __init__(self, files):
                self.files = files

            async def iter_files(self, **query):
                for file in self.files:
                    yield file

            @asynccontextmanager
            async def download_file(self, file_id):
                class Response:
                    async def iter_bytes(self):
                        yield b"ok"
                yield Response()

        file = {"id": "output", "filename": "repaired_calc.py", "downloadable": True, "size_bytes": 2}
        with TemporaryDirectory() as directory:
            root = Path(directory)
            for files in ([], [file, file], [{**file, "size_bytes": 65537}]):
                with self.assertRaises(RuntimeError):
                    await download_output(Client(files), "s", root)
            output = await download_output(Client([file]), "s", root)
            self.assertEqual(output.read_bytes(), b"ok")
            with self.assertRaises(FileExistsError):
                await download_output(Client([file]), "s", root)
            self.assertEqual(output.read_bytes(), b"ok")

    async def test_download_rejects_empty_truncated_and_oversized_bodies(self):
        for body, size in [(b"", 0), (b"partial", 20), (b"x" * 65537, 1)]:
            class Client:
                async def iter_files(self, **query):
                    yield {"id": "f", "filename": "repaired_calc.py", "downloadable": True, "size_bytes": size}

                @asynccontextmanager
                async def download_file(self, file_id):
                    class Response:
                        async def iter_bytes(self):
                            yield body
                    yield Response()

            with self.subTest(size=size), TemporaryDirectory() as directory:
                with self.assertRaises(RuntimeError):
                    await download_output(Client(), "s", Path(directory))
                self.assertEqual(list(Path(directory).iterdir()), [])

    async def test_cleanup_failure_does_not_hide_original_failure(self):
        args = argparse.Namespace(timeout=10, session_id=None, model="fake", keep_resources=False)
        with patch.dict("os.environ", {"MANGO_API_KEY": "test-key"}), \
             patch("main.AsyncMango", return_value=AsyncMock()), \
             patch("main.create_session", side_effect=RuntimeError("original failure")), \
             patch("main.Created.cleanup", side_effect=RuntimeError("cleanup failure")), \
             patch("sys.stderr") as stderr:
            with self.assertRaisesRegex(RuntimeError, "original failure"):
                await run(args)
            self.assertIn("cleanup failure", str(stderr.write.call_args_list))


class VerifierTests(unittest.TestCase):
    def test_missing_image_reports_the_setup_step(self):
        with patch("verify.subprocess.run", side_effect=FileNotFoundError):
            with self.assertRaisesRegex(RuntimeError, "docker pull python:3.12-alpine"):
                check_docker("python:3.12-alpine")

    def test_failed_checks_are_not_accepted_and_container_is_removed(self):
        with TemporaryDirectory() as directory:
            source = Path(directory) / "calc.py"
            source.write_text("invalid source")
            with patch("verify.subprocess.run") as execute:
                execute.return_value.returncode = 1
                with self.assertRaisesRegex(RuntimeError, "Independent checks failed"):
                    verify_output(source, "python:3.12-alpine")
            run = execute.call_args_list[0].args[0]
            self.assertIn("--network=none", run)
            self.assertIn("--read-only", run)
            self.assertIn("--user=65534:65534", run)
            self.assertEqual(execute.call_args_list[1].args[0][:3], ["docker", "rm", "--force"])

    def test_timeout_also_removes_the_container(self):
        with TemporaryDirectory() as directory:
            source = Path(directory) / "calc.py"
            source.write_text("while True: pass")
            with patch("verify.subprocess.run", side_effect=[subprocess.TimeoutExpired("docker", 20), None]) as execute:
                with self.assertRaises(subprocess.TimeoutExpired):
                    verify_output(source, "python:3.12-alpine")
            self.assertEqual(execute.call_args_list[1].args[0][:3], ["docker", "rm", "--force"])


if __name__ == "__main__":
    unittest.main()
