#!/usr/bin/env bash
# Documentation HTTP example. Only deletes resources created by this invocation.
set -euo pipefail

# region client
MANGO_BASE_URL=${MANGO_BASE_URL:-http://localhost:8080}
: "${MANGO_API_KEY:?Set MANGO_API_KEY before running this example}"
# endregion client

ENV_ID= AGENT_ID= SESSION_ID=
cleanup() {
  local result=$?
  if [[ -n "$SESSION_ID" ]]; then
    curl -fsS --max-time 30 -X DELETE -H "Authorization: Bearer $MANGO_API_KEY" \
      "$MANGO_BASE_URL/v1/sessions/$SESSION_ID" >/dev/null || result=1
  fi
  if [[ -n "$AGENT_ID" ]]; then
    curl -fsS --max-time 30 -X POST -H "Authorization: Bearer $MANGO_API_KEY" \
      "$MANGO_BASE_URL/v1/agents/$AGENT_ID/archive" >/dev/null || result=1
  fi
  if [[ -n "$ENV_ID" ]]; then
    curl -fsS --max-time 30 -X DELETE -H "Authorization: Bearer $MANGO_API_KEY" \
      "$MANGO_BASE_URL/v1/environments/$ENV_ID" >/dev/null || result=1
  fi
  exit "$result"
}
trap cleanup EXIT

# region environment
ENV_ID=$(curl -fsS --max-time 30 "$MANGO_BASE_URL/v1/environments" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Quickstart","config":{"type":"cloud"}}' | jq -er .id)
# endregion environment

# region agent
AGENT_ID=$(curl -fsS --max-time 30 "$MANGO_BASE_URL/v1/agents" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Assistant","model":"offline-fake","system":"Be concise."}' | jq -er .id)
# endregion agent

# region session
SESSION_ID=$(curl -fsS --max-time 30 "$MANGO_BASE_URL/v1/sessions" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg agent "$AGENT_ID" --arg environment "$ENV_ID" \
    '{agent:$agent,environment_id:$environment,title:"First session"}')" | jq -er .id)
# endregion session

# region stream
curl -fsS --max-time 30 "$MANGO_BASE_URL/v1/sessions/$SESSION_ID/events" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"events":[{"type":"user.message","content":[{"type":"text","text":"Hello, Mango!"}]}]}' \
  >/dev/null

# HTTP-only variant: poll durable history for this new Session's first turn.
completed=false
for ((attempt=0; attempt<60; attempt++)); do
  HISTORY=$(curl -fsS --max-time 30 \
    -H "Authorization: Bearer $MANGO_API_KEY" \
    "$MANGO_BASE_URL/v1/sessions/$SESSION_ID/events?order=asc&limit=100")
  if jq -e '.data | any(.type == "session.status_idle" and .stop_reason.type == "end_turn")' \
    <<<"$HISTORY" >/dev/null; then
    completed=true
    break
  fi
  sleep 0.5
done
if [[ "$completed" != true ]]; then
  echo 'No completed turn observed; inspect persisted history before retrying.' >&2
  exit 1
fi
jq '.data[] | select(.type == "agent.message") | .content' <<<"$HISTORY"
# endregion stream

# region history
curl -fsS --max-time 30 -H "Authorization: Bearer $MANGO_API_KEY" \
  "$MANGO_BASE_URL/v1/sessions/$SESSION_ID/events?order=asc&limit=100" | jq
# Follow next_page for longer histories; the SDK iterators do this for you.
# endregion history
jq -e '.data | any(.type == "agent.message")' <<<"$HISTORY" >/dev/null
echo 'Quickstart completed'
