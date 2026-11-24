# 17 Opt-in prompt logging, a logs viewer and retention

Type: task
Status: resolved
Blocked by: 04

Prompts and responses are not logged by default (spec, Security). A workspace
setting (04) turns logging on with a retention period. Logged bodies go to
their own table, never into `usage_events`, written off the request path like
usage, with secrets redacted and a size cap. `GET /api/logs` reads them back,
paged and filtered by route and time, and the retention job prunes them.
Turning logging off stops new logging; whether it also deletes what is stored
is decided here and stated in the API's response.

## Answer

- **The setting.** Workspace settings gain `prompt_logging_enabled` (off by
  default) and `prompt_log_retention_days` (1 to 30, 7 when omitted). The
  gateway reads them with the route, so there is no extra query.
- **The table.** `request_logs` is keyed by the usage event's id, so a log
  joins its usage event. It holds the client's request as received and what
  the client was sent: the assembled answer, streamed or not, as far as it
  got, or the error body. It also holds the outcome, because a broken stream
  is still a 200.
- **Redaction and cap.** `promptlog.Prepare` makes each side valid UTF-8, so
  Postgres never refuses one, and cuts it to 32 KiB on a UTF-8 boundary. A
  cut body then ends at the last space, quote or comma within 512 bytes, so
  a secret the cut split leaves no stub. It then masks secrets with a table
  of named scanners: bearer tokens, known key prefixes (OpenAI, Anthropic,
  Stripe, AWS, GitHub, GitLab, Slack, Google, NVIDIA and others), JWTs, PEM
  private key blocks, URL credentials, and the value of any field or
  variable whose name holds a secret word, quoted or bare, in JSON, in JSON
  inside a JSON string, and as text. Each scanner is a substring search plus
  a local parse, about 0.15 ms per 32 KiB side where the regular expressions
  took 6.8 ms. Redaction is pattern-based: a secret in free prose with no
  key prefix and no name is not caught.
- **Never delaying the response.** The gateway prepares a log on its own
  goroutine once the handler has returned, because net/http ends a
  non-streaming body and a stream only then. It cuts the answer's text to 32
  KiB before encoding it, so an 8 MiB answer is never encoded whole.
- **The writer.** Usage's queue became `batch.Queue[T]`, shared by
  `usage.Recorder` and `promptlog.Recorder`. The prompt log's queue is 256
  entries, a 16 MiB budget over two 32 KiB sides, rather than usage's 10000.
  Drops are counted by reason in `prompt_log.entries.dropped`. On shutdown
  both queues are flushed, together, before the pool closes.
- **The API.** `GET /api/logs?route=&from=&to=&cursor=&limit=` returns logs
  newest first, keyset paged, with RFC 3339 `from` and an exclusive `to`.
  `limit` is 20 by default and at most 50, and the page is encoded without
  HTML escaping: a worst-case page of 50 full logs allocates about 21 MiB to
  encode, where 200 escaped ones took 206 MiB. `DELETE /api/logs` deletes
  the workspace's logs now.
- **Turning logging off.** It stops new logs and deletes nothing. Stored
  logs expire by their retention. `GET /api/logs` says so in its `logging`
  object (`on_disable`). Neither turning logging off nor `DELETE /api/logs`
  reaches a log still queued: a request that ended in the last few seconds
  can log after either, and `on_disable` says so. A second `DELETE` a few
  seconds later removes it.
- **Retention.** Each workspace's logs are pruned hourly by its own
  retention period.

## Comments
