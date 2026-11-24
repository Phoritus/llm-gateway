// Package promptlog is opt-in prompt logging's write side: the redaction and
// cap a body goes through before it is queued, the queue that writes logs to
// Postgres off the request path, and the retention job that prunes them by
// each workspace's own retention. The read side is the store, behind
// GET /api/logs.
package promptlog
