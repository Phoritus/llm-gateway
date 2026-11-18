# 16 Periodic provider key health check

Type: task
Status: ready-for-agent
Blocked by: -

The spec's optional background job: test each credential now and then with
the adapter's `CheckKey` (it generates nothing) and record the result and
time on the credential, shown by `GET /api/credentials`, so a revoked key is
visible before a request has to fail over it. Spread the checks out, at most
one per credential a day, and never let a check move the router's skip
state: that stays driven by real traffic.

## Comments
