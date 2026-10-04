"""Local Mango HTTP-handler conformance; not evidence of a live model run."""

from __future__ import annotations

import asyncio
import os

from mango_sdk import APIError, AsyncMango, Mango, Upload


def main() -> None:
    url = os.environ["MANGO_SDK_TEST_URL"]
    key = os.environ["MANGO_SDK_TEST_KEY"]
    agents: list[str] = []
    environment_id: str | None = None
    session_id: str | None = None
    skill_input = {"type": "custom", "skill_id": "skill_reports", "version": "latest"}
    resolved_skill = {"type": "custom", "skill_id": "skill_reports", "version": "1759178010641129"}
    input_schema = {
        "type": "object", "properties": {"query": {"type": "string"}},
        "required": ["query"], "additionalProperties": False,
    }
    with Mango(base_url=url, api_key=key) as client:
        client.system.health()
        mcp_page = client.sessions.events.list("sesn_mcp_fixture")
        assert mcp_page["data"][0]["type"] == "agent.mcp_tool_result"
        assert mcp_page["data"][0]["file_id"] == "file_mcp_full"
        auto_page = client.sessions.events.list("sesn_auto_fixture")
        assert len(auto_page["data"]) == 6
        for index, event in enumerate(auto_page["data"]):
            assert event["type"] == ("agent.tool_use" if index < 3 else "agent.mcp_tool_use")
            expected = ("allow", "ask", "deny")[index % 3]
            assert event["evaluated_permission"] == expected
            evaluation = event["evaluation"]
            assert evaluation["type"] == "auto"
            assert evaluation["evaluated_permission"]["type"] == expected
            if expected != "allow":
                assert evaluation["evaluated_permission"]["reason_code"] == ("indeterminate" if expected == "ask" else "high_risk")
        payload = b"mango\x00\xff"
        uploaded = client.files.upload(file=Upload("result.bin", payload, "application/octet-stream"))
        try:
            assert set(uploaded) == {"id", "type", "created_at", "filename", "mime_type", "size_bytes", "checksum_sha256"}
            assert uploaded["size_bytes"] == len(payload)
            import hashlib
            assert uploaded["checksum_sha256"] == hashlib.sha256(payload).hexdigest()
            with client.files.download(uploaded["id"]) as stream:
                assert b"".join(stream.iter_bytes()) == payload
        finally:
            client.files.delete(uploaded["id"])
        try:
            environment = client.environments.create(name="python-sdk-conformance", config={"type": "self_hosted"})
            environment_id = environment["id"]
            check = client.environments.work.create(environment_id, data={"type": "healthcheck"})
            assert check["data"] == {"type": "healthcheck"}
            assert check["expires_at"] is not None and check["result"] is None
            done = client.environments.work.complete(environment_id, check["id"], status="failed", message="conformance diagnostic")
            assert done["result"] == {"status": "failed", "message": "conformance diagnostic"}
            checks = client.environments.work.list(environment_id)
            assert checks["data"][0]["data"] == {"type": "healthcheck"}
            for suffix in ("one", "two"):
                agent = client.agents.create(name="python-sdk-" + suffix, model="sdk-conformance", skills=[skill_input], tools=[{
                        "type": "custom", "name": "lookup", "description": "Look up a record",
                        "input_schema": input_schema,
                    }, {"type": "agent_toolset_20260401", "default_config": {"enabled": False}, "configs": [{"name": "read", "enabled": True, "permission_policy": {"type": "auto"}}]}])
                agents.append(agent["id"])
                assert client.agents.retrieve(agent["id"])["name"] == "python-sdk-" + suffix
                assert agent["tools"][0]["input_schema"] == input_schema
                assert agent["tools"][1]["configs"][0]["permission_policy"] == {"type": "auto"}
                assert agent["skills"] == [resolved_skill]
                assert client.agents.retrieve(agent["id"])["skills"] == [resolved_skill]
            page = client.agents.list(limit=1)
            assert len(page["data"]) == 1 and page["next_page"]
            listed = {item["id"] for item in client.agents.iter(limit=1)}
            assert set(agents).issubset(listed)
            coordinator = client.agents.create(
                name="python-sdk-lead", model="sdk-conformance",
                skills=[skill_input], tools=[{"type": "agent_toolset_20260401"}],
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
            assert session["agent"]["skills"] == [resolved_skill]
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
