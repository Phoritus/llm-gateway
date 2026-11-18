# 17 Opt-in prompt logging, a logs viewer and retention

Type: task
Status: ready-for-agent
Blocked by: 04

Prompts and responses are not logged by default (spec, Security). A workspace
setting (04) turns logging on with a retention period. Logged bodies go to
their own table, never into `usage_events`, written off the request path like
usage, with secrets redacted and a size cap. `GET /api/logs` reads them back,
paged and filtered by route and time, and the retention job prunes them.
Turning logging off stops new logging; whether it also deletes what is stored
is decided here and stated in the API's response.

## Comments
