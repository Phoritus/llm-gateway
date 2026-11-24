# 09 Custom OpenAI-compatible endpoints

Type: task
Status: resolved
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

## Answer

- `provider.VendorCustom`; migration `00011` adds it and `base_url`, which a
  `custom` credential must have and no other may.
- `POST /api/credentials` takes `base_url` for `custom` only: https, a host,
  no user, query or fragment, and no address `egress.Check` refuses or
  `localhost`. A hostname is checked again on every dial, by `egress`.
- The base URL is part of the sealed key's AAD: changing it in the table makes
  the key fail to open. Other vendors' AAD is unchanged, so stored keys still
  open.
- `openai.NewCustom` is the adapter, one client for every custom credential, the
  base URL coming from `provider.Credential.BaseURL`, every call through the
  `egress` client. `POST /api/credentials/{id}/test` uses it.
- The breaker keys a custom target on its credential.
- From the security review: an upstream a user chose could send one SSE
  event, or one answer, without end and exhaust the shared process's memory.
  `sse.MaxEvent` (4 MiB) and `provider.MaxAnswerBytes` (8 MiB) now bound
  both, for every vendor. Error details leave out the URL, whose path may
  carry a token.
