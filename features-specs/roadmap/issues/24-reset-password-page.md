# 24 Serve the reset-password page the reset mail links to

Status: ready-for-agent
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
