"""Run a specialist team against Mango. Requires a configured model and Environment."""
from __future__ import annotations

import os

import httpx

from mango_sdk import Mango, models


def run() -> None:
    model = os.environ["MANGO_MODEL_ID"]
    environment_id = os.environ["MANGO_ENVIRONMENT_ID"]
    agent_ids: list[str] = []
    session_id: str | None = None
    with Mango(
        base_url=os.environ.get("MANGO_BASE_URL", "http://localhost:8080"),
        api_key=os.environ["MANGO_API_KEY"], stream_timeout=httpx.Timeout(180.0),
    ) as client:
        try:
            # region team
            researcher = client.agents.create(
                name="researcher", model=model,
                system="Analyze the proposal and report concrete benefits and tradeoffs.",
            )
            agent_ids.append(researcher["id"])
            reviewer = client.agents.create(
                name="reviewer", model=model,
                system="Review the proposal independently and identify risks and missing assumptions.",
            )
            agent_ids.append(reviewer["id"])
            roster: list[models.MultiagentRosterEntryInput] = [
                {"type": "agent", "id": researcher["id"], "version": researcher["version"]},
                {"type": "agent", "id": reviewer["id"], "version": reviewer["version"]},
            ]
            advisor = os.environ.get("MANGO_ADVISOR_MODEL")
            if advisor:
                roster.append({"type": "advisor", "model": advisor})
            coordinator = client.agents.create(
                name="lead", model=model,
                system="Delegate to both specialists, wait for their reports, then synthesize. "
                       "For follow-ups, reuse the existing specialist threads.",
                multiagent={"type": "coordinator", "agents": roster},
            )
            agent_ids.append(coordinator["id"])
            session = client.sessions.create(agent=coordinator["id"], environment_id=environment_id)
            session_id = session["id"]
            # endregion team

            def turn(text: str) -> None:
                # region observe
                with client.sessions.events.stream(session["id"]) as stream:
                    client.sessions.events.send(session["id"], events=[{
                        "type": "user.message", "content": [{"type": "text", "text": text}],
                    }])
                    for envelope in stream:
                        event = envelope.data
                        if event["type"] == "agent.message":
                            print(event["content"])
                        if event["type"] == "session.status_idle":
                            if event["stop_reason"]["type"] != "end_turn":
                                raise RuntimeError("Session needs attention; inspect its persisted events")
                            return
                        if event["type"] in ("session.status_terminated", "session.deleted"):
                            raise RuntimeError("Session terminated before completing the turn")
                raise RuntimeError("Stream disconnected; reconcile persisted events before resending")
                # endregion observe

            turn(os.environ.get("MANGO_TASK", "Compare a monolith and microservices for a three-person team."))
            turn("Ask the existing reviewer thread to challenge its earlier conclusion, then summarize.")
            # region threads
            for thread in client.sessions.threads.iter(session["id"]):
                print(thread["id"], thread["status"])
                for event in client.sessions.threads.events.iter(session["id"], thread["id"]):
                    if event["type"] == "agent.message":
                        print(event["content"])
            # endregion threads
        finally:
            try:
                if session_id:
                    client.sessions.delete(session_id)
            finally:
                for agent_id in reversed(agent_ids):
                    client.agents.archive(agent_id)


if __name__ == "__main__":
    run()
