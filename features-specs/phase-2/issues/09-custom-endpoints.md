# 09 Custom OpenAI-compatible endpoints

Type: task
Status: ready-for-agent
Blocked by: 08

A credential with vendor `custom` carries a base URL as well as its secret:
an https URL to an OpenAI-compatible server (a self-hosted model, or a vendor
not in `openai.Endpoints`). The URL is validated when the credential is
stored and bound into the credential's AAD, so it cannot be swapped under a
stored key, and every call goes through the guard from 08. The adapter is the
existing `provider/openai`, built per credential instead of per vendor.
`POST /api/credentials/{id}/test` works for it. Breaker and skip state key on
the credential, not on the shared `custom` vendor, so one user's broken
server cannot open the breaker for everyone else's.

## Comments
