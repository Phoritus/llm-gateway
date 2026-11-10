// Package auth handles gateway keys, sessions and password hashing.
//
// What is implemented so far is the control plane half: argon2id password
// hashing, opaque server-side sessions carried in an HttpOnly cookie, the
// CSRF token bound to each session, the email-verification and password-reset
// tokens, and the middleware that resolves a request's identity. Gateway keys
// for the /v1 data plane come next and
// belong in this package too, since they share the same hash-and-compare
// shape.
//
// Four rules hold across everything here:
//
//   - A secret is stored only as a hash. Passwords use argon2id because they
//     are low-entropy and attacker-guessable; session, verification and reset
//     tokens use plain SHA-256 because they are already 256 bits of randomness
//     and a lookup has to be cheap.
//
//   - A caller's workspace comes from the session row, resolved on every
//     request, never from anything the client sends. The resolving query joins
//     workspace_members, so losing a membership takes effect immediately, with
//     no cache to invalidate.
//
//   - A policy value is received, never defaulted. internal/config validates
//     every duration in Config and this package substitutes nothing, so the
//     lifetimes the service runs on are the ones a deployment set.
//
//   - The password rules live in HashPassword, the one door to a stored
//     password. A caller translates a refusal through IsPasswordRejected
//     rather than applying the rules itself, which is what keeps the ordering
//     of "is the current password right" against "is the new one allowed" a
//     single decision instead of one per call site.
//
//     Signup is the one caller that also pre-checks, with ValidatePassword,
//     before it spends a rate-limit bucket: at ten signups an hour per
//     address, charging for a password that was never going to be accepted
//     would lock someone out of registering over a typo. It is a pre-check and
//     not the enforcement point -- HashPassword still applies the rules, and
//     the IsPasswordRejected branch behind it is unreachable only for as long
//     as the pre-check stays. There is deliberately no third caller: a handler
//     that judges a password it is not about to hash is deciding the ordering
//     this bullet exists to keep in one place.
//
// Email verification is enforced, not merely recorded: RequireVerifiedEmail
// gates every route that spends the service, and only /me, logout, logout-all
// and change-password are exempt. When gateway keys land they inherit the same
// rule -- a key may not be minted by an unconfirmed account, which is what
// carries the policy into /v1, where there is no session to check.
package auth
