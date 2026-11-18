# 18 Budgets

Type: grilling
Status: needs-triage
Blocked by: 04

The spec lists budgets for Phase 2 and says nothing more. Questions for the
user: budgets in tokens or in money (money needs a price table per model,
which goes stale like the free-quota catalog), per workspace, per route or
per key, per day or per month, and whether a spent budget refuses requests
(429 with its own code) or only warns. The rate limiter's
reserve-then-reconcile is the obvious place to enforce one.

## Comments
