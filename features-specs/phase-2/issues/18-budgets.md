# 18 Budgets

Type: task
Status: resolved
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

## Answer

Built on `feat/budgets`.

- **Prices:** `model_prices` (migration 00014), USD per million input,
  output and cached-input tokens, with `checked_on` and `source`, seeded only
  with prices read on each vendor's own page on 2026-11-24 (OpenAI, Anthropic,
  Gemini, Groq; Mistral's pricing page gives no API model ids, so it has no
  rows yet). Loaded into `budget.Prices` and reloaded every 10 minutes.
- **Cost** is computed after the answer from the reported usage, in micro-USD,
  rounded up per request. A generation that started and reported nothing is
  priced at the token estimate, as prompt tokens. A free-quota target and a
  cache hit cost nothing.
- **A model with no price row costs nothing** and the response says so:
  `X-Budget-Cost-Basis: unpriced`, and a null `cost_usd` and an
  `unpriced_requests` count in `GET /api/usage`. Refusing it would break custom
  endpoints and every model newer than the table; a fallback price would be a
  number nobody chose.
- **Budgets** per workspace (`monthly_budget_usd` and `budget_action` in the
  workspace settings) and per gateway key (on `POST /api/gateway-keys` and
  `PUT /api/gateway-keys/{id}/budget`), per UTC calendar month, resetting at
  00:00 UTC on the 1st. Spend lives in Redis per scope and month.
- **Block** answers 429 `insufficient_quota` / `budget_exceeded` once spent,
  checked on a cache miss right before routing, so no upstream is called and
  a cache hit is still served. No `Retry-After`: the wait is weeks, and SDKs
  sleep on it. **Warn** keeps serving. Both mail the owner once a month at 80%
  and at 100%.
- **Visible:** `X-Budget-Remaining-USD` on each request a budget covers; cost
  per day and per request, and the workspace's budget and spend this month, in
  `GET /api/usage`; each key's spend in `GET /api/gateway-keys`.
- Money is checked, then added, never reserved, so a budget can be overshot by
  the requests already in flight when it fills.
