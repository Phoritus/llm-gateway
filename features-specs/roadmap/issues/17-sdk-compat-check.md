# 17 Compatibility check: the official OpenAI SDK and one IDE client

Status: ready-for-agent
Blocked by: 06

From the spec's testing list. Point the OpenAI Python SDK (`base_url=.../v1`
with a gateway key) and one IDE client at the gateway, and check that
streaming, non-streaming and an error each behave. This can start against the
mock as soon as /v1 accepts keys.

## Comments
