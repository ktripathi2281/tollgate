"""Manual M1 check: a stock OpenAI SDK gets a completion from the gateway.

One-time setup, from the repo root inside WSL. Ubuntu needs its venv
package first (sudo apt install python3-venv):

    python3 -m venv .venv
    .venv/bin/pip install openai

Then start the gateway with `make run` and, in another terminal:

    .venv/bin/python scripts/openai_sdk_check.py

Windows Python works too, since WSL forwards localhost:8080 to Windows:
create the venv with `python -m venv .venv` and run `.venv\\Scripts\\python`.

Set GATEWAY_URL to point somewhere other than http://localhost:8080/v1.
Last run: openai 3.26.0, 2026-10-07.
"""

import os
import sys

import openai

BASE_URL = os.environ.get("GATEWAY_URL", "http://localhost:8080/v1")

# The gateway has no auth until M4, but the SDK insists on a key.
client = openai.OpenAI(base_url=BASE_URL, api_key="not-checked-yet", max_retries=0)

ok = True

models = [m.id for m in client.models.list()]
print("models:       ", models)
ok = ok and "mock-fast" in models

resp = client.chat.completions.create(
    model="mock-fast",
    messages=[{"role": "user", "content": "Say hello to the gateway."}],
    max_completion_tokens=12,
)
choice = resp.choices[0]
print("id:           ", resp.id)
print("model:        ", resp.model)
print("finish_reason:", choice.finish_reason)
print("usage:        ", resp.usage.prompt_tokens, "prompt +", resp.usage.completion_tokens, "completion tokens")
print("content:      ", choice.message.content)
print("request id:   ", getattr(resp, "_request_id", None))
ok = ok and resp.object == "chat.completion" and choice.message.role == "assistant" and bool(choice.message.content)

# Errors arrive as the SDK's typed exceptions, parsed from the error envelope.
try:
    client.chat.completions.create(model="no-such-model", messages=[{"role": "user", "content": "hi"}])
    print("unknown model: expected an error, got a completion")
    ok = False
except openai.NotFoundError as e:
    print("unknown model:", e.status_code, e.code)
    ok = ok and e.code == "model_not_found"

print("OK" if ok else "FAILED")
sys.exit(0 if ok else 1)
