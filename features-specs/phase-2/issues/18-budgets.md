# 18 Budgets

Type: task
Status: ready-for-agent
Blocked by: 04

The spec lists budgets for Phase 2 and says nothing more. Questions for the
user: budgets in tokens or in money (money needs a price table per model,
which goes stale like the free-quota catalog), per workspace, per route or
per key, per day or per month, and whether a spent budget refuses requests
(429 with its own code) or only warns. The rate limiter's
reserve-then-reconcile is the obvious place to enforce one.

## Comments

- 2026-11-18, the owner decided:
  - Budgets are in **money**, as other gateways do.
  - They apply **per workspace and per gateway key**.
  - Each budget's owner chooses **block or warn**, and **block** is the default.

## Scope

- A price table next to `free_quota_catalog`: per vendor and model, USD per
  million input and per million output tokens, cached input where a vendor
  prices it. Seeded and reloaded the way the free-quota catalog is, and
  correctable by `UPDATE`. Prices go stale like the free limits (see
  CLAUDE.md), so each row records when it was checked.
- Cost is computed from the usage the upstream reports, after the answer.
  The rate limiter's reserve-then-reconcile is where an estimate is held
  before the call and corrected after. A free-tier answer (a free-quota
  target) and a cache hit cost nothing.
- A model with no price row: decide here whether it costs nothing, is
  refused on a blocking budget, or is counted at a stated fallback price,
  and make the response say which.
- Budgets per workspace and per gateway key, monthly with a stated reset
  (UTC calendar month unless decided otherwise), set through the workspace
  settings API (04) and the gateway keys API.
- **Block:** refuse with 429 and code `budget_exceeded` once spent, before
  any upstream call. **Warn:** keep serving and mail the workspace owner once
  per period at 80% and 100%.
- Spend is visible: `GET /api/usage` carries cost, and the request reports
  the remaining budget in a response header.
