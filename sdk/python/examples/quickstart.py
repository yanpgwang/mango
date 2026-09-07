"""Documentation example for the offline Compose stack; no hosted credentials."""

# region client
import os

import httpx
from mango_sdk import Mango

client = Mango(
    base_url=os.environ.get("MANGO_BASE_URL", "http://localhost:8080"),
    api_key=os.environ["MANGO_API_KEY"],
    stream_timeout=httpx.Timeout(60.0),
)
# endregion client

environment_id: str | None = None
agent_id: str | None = None
session_id: str | None = None
try:
    # region environment
    environment = client.environments.create(name="Quickstart")
    # endregion environment
    environment_id = environment["id"]

    # region agent
    agent = client.agents.create(name="Assistant", model="offline-fake", system="Be concise.")
    # endregion agent
    agent_id = agent["id"]

    # region session
    session = client.sessions.create(agent=agent["id"], environment_id=environment["id"], title="First session")
    # endregion session
    session_id = session["id"]

    # region stream
    # Enter the stream context before sending: it is live-only, not a replay.
    with client.sessions.events.stream(session["id"]) as stream:
        client.sessions.events.send(session["id"], events=[{
            "type": "user.message",
            "content": [{"type": "text", "text": "Hello, Mango!"}],
        }])
        completed = False
        for envelope in stream:
            event = envelope.data
            if event["type"] == "agent.message":
                print(event["content"])
            if event["type"] == "session.status_idle":
                if event["stop_reason"]["type"] != "end_turn":
                    raise RuntimeError("The turn requires attention")
                completed = True
                break
        if not completed:
            raise RuntimeError("Stream ended before completion; reconcile persisted history")
    # endregion stream

    # region history
    history = list(client.sessions.events.iter(session["id"], order="asc", limit=100))
    print(f"Persisted events: {len(history)}")
    # endregion history
    assert any(event["type"] == "agent.message" for event in history)
    print("Quickstart completed")
finally:
    # Only clean up the resources created by this invocation.
    try:
        if session_id:
            client.sessions.delete(session_id)
    finally:
        try:
            if agent_id:
                client.agents.archive(agent_id)
        finally:
            try:
                if environment_id:
                    client.environments.delete(environment_id)
            finally:
                client.close()
