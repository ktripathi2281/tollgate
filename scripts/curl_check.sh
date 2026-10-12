#!/bin/sh
# Manual check with curl: one non-streaming chat completion from the
# mock-fast alias, with the response headers. Start the gateway first with
# `make run`, and set TOLLGATE_KEY to a key from `tollgate keys create`.
# Set GATEWAY_URL to point somewhere other than localhost:8080.
set -eu

URL="${GATEWAY_URL:-http://localhost:8080/v1}"
KEY="${TOLLGATE_KEY:?set TOLLGATE_KEY to a key from tollgate keys create}"

curl -sS -i "$URL/chat/completions" \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "mock-fast",
    "messages": [{"role": "user", "content": "Say hello to the gateway."}],
    "max_completion_tokens": 12
  }'
echo
