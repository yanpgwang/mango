"""Local Mango HTTP-handler conformance; not evidence of a live model run."""

from __future__ import annotations

import asyncio
import os

from mango_sdk import APIError, AsyncMango, Mango


def main() -> None:
    url = os.environ["MANGO_SDK_TEST_URL"]
    key = os.environ["MANGO_SDK_TEST_KEY"]
    agents: list[str] = []
    environment_id: str | None = None
    session_id: str | None = None
    input_schema = {
        "type": "object", "properties": {"query": {"type": "string"}},
        "required": ["query"], "additionalProperties": False,
    }
    with Mango(base_url=url, api_key=key) as client:
        client.health()
        try:
            environment = client.create_environment(body={
                "name": "python-sdk-conformance", "config": {"type": "cloud"},
            })
            environment_id = environment["id"]
            for suffix in ("one", "two"):
                agent = client.create_agent(body={
                    "name": "python-sdk-" + suffix, "model": "sdk-conformance",
                    "tools": [{
                        "type": "custom", "name": "lookup", "description": "Look up a record",
                        "input_schema": input_schema,
                    }],
                })
                agents.append(agent["id"])
                assert client.get_agent(agent["id"])["name"] == "python-sdk-" + suffix
                assert agent["tools"][0]["input_schema"] == input_schema
            page = client.list_agents(limit=1)
            assert len(page["data"]) == 1 and page["next_page"]
            listed = {item["id"] for item in client.iter_agents(limit=1)}
            assert set(agents).issubset(listed)
            session = client.create_session(body={
                "agent": {"type": "agent", "id": agents[0]},
                "environment_id": environment_id,
            })
            session_id = session["id"]
            with client.stream_session_events(session_id) as stream:
                sent = client.send_session_events(session_id, body={"events": [{
                    "type": "user.message", "content": [{"type": "text", "text": "sdk test"}],
                }]})
                assert len(sent["data"]) == 1
                observed = False
                for envelope in stream:
                    if envelope.data.get("id") == sent["data"][0]["id"]:
                        observed = True
                        break
                assert observed, "A ready subscription must receive the submitted event"
            history = list(client.iter_session_events(session_id, order="asc"))
            assert any(item["type"] == "user.message" for item in history)
            try:
                client.get_session("sesn_python_missing")
            except APIError as error:
                assert error.status_code == 404
                assert error.type == "not_found_error"
                assert error.request_id
            else:
                raise AssertionError("Missing Session must return a typed 404 error")

            async def check_async() -> None:
                async with AsyncMango(base_url=url, api_key=key) as async_client:
                    await async_client.health()
                    assert session_id is not None
                    assert (await async_client.get_session(session_id))["id"] == session_id

            asyncio.run(check_async())
        finally:
            if session_id:
                client.delete_session(session_id)
            for agent_id in agents:
                client.archive_agent(agent_id)
            if environment_id:
                client.delete_environment(environment_id)
    print("Python SDK local Mango HTTP conformance passed")


if __name__ == "__main__":
    main()
