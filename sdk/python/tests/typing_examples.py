"""Checked by mypy: public methods return useful discriminated request/response types."""

from mango_sdk import AsyncMango, Mango, Upload, models


def sync_types(client: Mango) -> None:
    body: models.AgentCreateRequest = {"name": "example", "model": "test-model", "system": None}
    tool: models.CustomTool = {
        "type": "custom", "name": "lookup", "description": "Look up a record",
        "input_schema": {
            "type": "object",
            "properties": {"query": {"type": "string"}},
            "required": ["query"],
            "additionalProperties": False,
            "$defs": {"identifier": {"type": "string"}},
        },
    }
    body["tools"] = [tool]
    agent: models.Agent = client.agents.create(**body)
    for candidate in client.agents.iter(limit=1, include_archived=False):
        name: str = candidate["name"]
        print(name, agent["id"])
    upload: models.FileUploadRequest = {"file": Upload("data.txt", b"hello")}
    client.files.upload(**upload)
    event: models.UserMessageEventInput = {
        "type": "user.message", "content": [{"type": "text", "text": "hello"}],
    }
    client.sessions.events.send("sesn_example", events=[event])
    with client.sessions.events.stream("sesn_example") as stream:
        for envelope in stream:
            frame: models.EventStreamFrame = envelope.data
            if frame["type"] == "event_delta":
                text: str = frame["delta"]["content"]["text"]
                print(text)
            break


async def async_types(client: AsyncMango) -> None:
    page: models.SessionList = await client.sessions.list(statuses=["idle"])
    async for session in client.sessions.iter(limit=1):
        print(session["id"], page["next_page"])
    async with client.sessions.events.stream("sesn_example") as stream:
        async for event in stream:
            print(event.data["type"])
            break
    async with client.files.download("file_example") as download:
        data: bytes = await download.read()
        print(len(data))
