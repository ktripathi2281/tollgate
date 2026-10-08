#!/bin/sh
# Manual check with curl: a streamed chat completion from the mock-fast
# alias. -N turns off curl's buffering, so the events print as they arrive,
# one word every 10ms with the dev config. Start the gateway first with
# `make run`. Set GATEWAY_URL to point somewhere other than localhost:8080,
# and MODEL=mock-flaky to see a stream fail partway through.
set -eu

URL="${GATEWAY_URL:-http://localhost:8080/v1}"
MODEL="${MODEL:-mock-fast}"

curl -sS -N -i "$URL/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "'"$MODEL"'",
    "stream": true,
    "stream_options": {"include_usage": true},
    "messages": [{"role": "user", "content": "Say hello to the gateway."}],
    "max_completion_tokens": 8
  }'
echo
