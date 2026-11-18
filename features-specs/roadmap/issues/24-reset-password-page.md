# 24 Serve the reset-password page the reset mail links to

Status: resolved
Blocked by: -

The reset mail links to `PUBLIC_BASE_URL/reset-password?token=…`, a page the
spec leaves to a dashboard that does not exist. Production answers it 404, so
a user who forgets their password cannot use the link (found while checking
ticket 16 on 2026-11-18). The token itself works: `POST
/api/auth/reset-password` with it resets the password.

Build: `GET /reset-password` on the gateway serves one small self-contained
HTML page (no external scripts, a strict CSP, `Referrer-Policy: no-referrer`
so the token never leaves in a Referer) with a new-password form that posts
the token and password to `/api/auth/reset-password` from the same origin, and
shows the API's error messages (`invalid_token`, password rules). Production
must allow its own origin for that POST: either `PUBLIC_BASE_URL`'s origin is
always allowed by the same-origin guard, or the deploy adds it to
`ALLOWED_ORIGINS`; decide which. When a dashboard lands, it takes the path
over.

## Comments

## Answer

`GET /reset-password` serves one page embedded in the binary
(`internal/api/resetpage/`). Its script and style are inlined, and the CSP
allows exactly their SHA-256 hashes, computed from the embedded bytes when the
binary starts, so editing `page.js` cannot leave the policy stale. The other
directives are `default-src 'none'`, `connect-src 'self'`, `form-action 'none'`,
`base-uri 'none'` and `frame-ancestors 'none'`, with `Referrer-Policy:
no-referrer` and `Cache-Control: no-store`. The script takes the token out of
the address bar with `history.replaceState` before anything else runs.

Origin decision: `PUBLIC_BASE_URL`'s origin is always allowed (`config.Load`
appends it), rather than a deploy step adding it to `ALLOWED_ORIGINS`. The
page is served by the gateway, so its origin is a fact of the deployment, and
a step a deploy can forget would break password reset with nothing in the
logs to say why. `ALLOWED_ORIGINS` is therefore no longer required in
production. It lists only additional origins.

`no-referrer` does not cost the POST its Origin header. The Fetch spec's
rule that a no-referrer POST sends `Origin: null` applies only to requests
whose mode is not `cors`, and `fetch()` defaults to `cors`, so a same-origin
`fetch` sends the real origin. Edge confirmed it, so the page sets no
per-request referrer policy.

Checked end to end in headless Edge against a running gateway: a missing
token, a mismatch caught before any request, the server's password rule
message, a successful reset, then a reused link refused as `invalid_token`.
The new password logs in, the old one is refused, the access log records the
path without the token, and there were no CSP violations.
