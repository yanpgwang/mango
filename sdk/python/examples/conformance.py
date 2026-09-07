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
        client.system.health()
        try:
            environment = client.environments.create(name="python-sdk-conformance", config={"type": "cloud"})
            environment_id = environment["id"]
            for suffix in ("one", "two"):
                agent = client.agents.create(name="python-sdk-" + suffix, model="sdk-conformance", tools=[{
                        "type": "custom", "name": "lookup", "description": "Look up a record",
                        "input_schema": input_schema,
                    }])
                agents.append(agent["id"])
                assert client.agents.retrieve(agent["id"])["name"] == "python-sdk-" + suffix
                assert agent["tools"][0]["input_schema"] == input_schema
            page = client.agents.list(limit=1)
            assert len(page["data"]) == 1 and page["next_page"]
            listed = {item["id"] for item in client.agents.iter(limit=1)}
            assert set(agents).issubset(listed)
            coordinator = client.agents.create(
                name="python-sdk-lead", model="sdk-conformance",
                multiagent={"type": "coordinator", "agents": [
                    {"type": "agent", "id": agents[0], "version": 1},
                    agents[1], {"type": "self"},
                    {"type": "advisor", "model": "review-model"},
                ]},
            )
            agents.append(coordinator["id"])
            assert coordinator["multiagent"]["agents"][0] == {
                "type": "agent", "id": agents[0], "version": 1,
            }
            assert coordinator["multiagent"]["agents"][-1] == {
                "type": "advisor", "model": "review-model",
            }
            session = client.sessions.create(agent={"type": "agent", "id": coordinator["id"]}, environment_id=environment_id)
            session_id = session["id"]
            with client.sessions.events.stream(session_id) as stream:
                sent = client.sessions.events.send(session_id, events=[{
                    "type": "user.message", "content": [{"type": "text", "text": "sdk test"}],
                }])
                assert len(sent["data"]) == 1
                observed = False
                for envelope in stream:
                    if envelope.data.get("id") == sent["data"][0]["id"]:
                        observed = True
                        break
                assert observed, "A ready subscription must receive the submitted event"
            history = list(client.sessions.events.iter(session_id, order="asc"))
            assert any(item["type"] == "user.message" for item in history)
            try:
                client.sessions.retrieve("sesn_python_missing")
            except APIError as error:
                assert error.status_code == 404
                assert error.type == "not_found_error"
                assert error.request_id
            else:
                raise AssertionError("Missing Session must return a typed 404 error")

            async def check_async() -> None:
                async with AsyncMango(base_url=url, api_key=key) as async_client:
                    await async_client.system.health()
                    assert session_id is not None
                    assert (await async_client.sessions.retrieve(session_id))["id"] == session_id

            asyncio.run(check_async())
        finally:
            if session_id:
                client.sessions.delete(session_id)
            for agent_id in agents:
                client.agents.archive(agent_id)
            if environment_id:
                client.environments.delete(environment_id)
    print("Python SDK local Mango HTTP conformance passed")


if __name__ == "__main__":
    main()
