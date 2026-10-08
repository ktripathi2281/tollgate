"""Manual check: a stock OpenAI SDK works against the gateway.

It lists models, gets a completion, streams one, and checks that errors
(an unknown model, and a stream that fails partway) arrive as the SDK's own
exceptions. It expects the dev config.yaml, with the mock-fast and
mock-flaky aliases.

One-time setup, from the repo root inside WSL. Ubuntu needs its venv
package first (sudo apt install python3-venv):

    python3 -m venv .venv
    .venv/bin/pip install openai

Then start the gateway with `make run` and, in another terminal:

    .venv/bin/python scripts/openai_sdk_check.py

Windows Python works too, since WSL forwards localhost:8080 to Windows:
create the venv with `python -m venv .venv` and run `.venv\\Scripts\\python`.

Set GATEWAY_URL to point somewhere other than http://localhost:8080/v1.
Last run: openai 3.26.0, 2026-10-08.
"""

import os
import sys

import openai

BASE_URL = os.environ.get("GATEWAY_URL", "http://localhost:8080/v1")
MESSAGES = [{"role": "user", "content": "Say hello to the gateway."}]

# The gateway has no auth until M4, but the SDK insists on a key.
client = openai.OpenAI(base_url=BASE_URL, api_key="not-checked-yet", max_retries=0)

ok = True


def check(name, passed):
    global ok
    ok = ok and passed
    if not passed:
        print("  FAILED:", name)


models = [m.id for m in client.models.list()]
print("models:       ", models)
check("mock-fast is listed", "mock-fast" in models)

# A complete response.
resp = client.chat.completions.create(model="mock-fast", messages=MESSAGES, max_completion_tokens=12)
choice = resp.choices[0]
print("id:           ", resp.id)
print("model:        ", resp.model)
print("finish_reason:", choice.finish_reason)
print("usage:        ", resp.usage.prompt_tokens, "prompt +", resp.usage.completion_tokens, "completion tokens")
print("content:      ", choice.message.content)
print("request id:   ", getattr(resp, "_request_id", None))
check("completion shape", resp.object == "chat.completion" and choice.message.role == "assistant" and bool(choice.message.content))

# A stream: the SDK yields chunks as they arrive.
pieces, finish, usage = [], None, None
stream = client.chat.completions.create(
    model="mock-fast",
    messages=MESSAGES,
    max_completion_tokens=12,
    stream=True,
    stream_options={"include_usage": True},
)
for chunk in stream:
    if chunk.usage:
        usage = chunk.usage
    for c in chunk.choices:
        if c.delta.content:
            pieces.append(c.delta.content)
        if c.finish_reason:
            finish = c.finish_reason
print("stream:       ", len(pieces), "content chunks, finish", finish,
      "| usage", usage and (usage.prompt_tokens, usage.completion_tokens))
print("streamed text:", "".join(pieces))
check("stream", len(pieces) == 12 and finish == "length" and usage is not None and usage.completion_tokens == 12)

# An unknown model is an ordinary error response.
try:
    client.chat.completions.create(model="no-such-model", messages=MESSAGES)
    check("unknown model raises", False)
except openai.NotFoundError as e:
    print("unknown model:", e.status_code, e.code)
    check("unknown model code", e.code == "model_not_found")

# A stream that fails after it started ends with an error event, which the
# SDK raises as an APIError partway through the iteration.
received = 0
try:
    for chunk in client.chat.completions.create(model="mock-flaky", messages=MESSAGES, stream=True):
        received += sum(1 for c in chunk.choices if c.delta.content)
    check("mid-stream failure raises", False)
except openai.APIError as e:
    print("mid-stream failure after", received, "content chunks:", type(e).__name__, e.code, "-", e.message)
    check("mid-stream failure", received == 4 and e.code == "upstream_error")

print("OK" if ok else "FAILED")
sys.exit(0 if ok else 1)
