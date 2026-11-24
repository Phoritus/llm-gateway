package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RequestLog is one logged request of a workspace that turned prompt logging
// on. Request and Response are already redacted and capped.
type RequestLog struct {
	// ID is the request's usage event id.
	ID                string
	WorkspaceID       string
	KeyID             string
	RouteAlias        string
	Vendor            string
	Model             string
	Status            int
	Outcome           Outcome
	CreatedAt         time.Time
	Request           string
	Response          string
	RequestTruncated  bool
	ResponseTruncated bool
}

// LogCursor is the position after which a page of logs starts: the last log
// of the page before, in newest-first order.
type LogCursor struct {
	CreatedAt time.Time
	ID        string
}

// LogQuery narrows a workspace's logs. A zero From or To leaves that end
// open; To is exclusive. A nil After starts at the newest.
type LogQuery struct {
	Route    string
	From, To time.Time
	After    *LogCursor
	Limit    int
}

// WriteRequestLogs stores logs in one statement. A log whose id is already
// stored is skipped, so a retried batch stores each once.
func (s *Store) WriteRequestLogs(ctx context.Context, logs []RequestLog) error {
	if len(logs) == 0 {
		return nil
	}
	n := len(logs)
	var (
		ids, ws, keys, route, vendor, model, outcome = make([]string, n), make([]string, n), make([]string, n),
			make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		status         = make([]int32, n)
		created        = make([]time.Time, n)
		request, resp  = make([]string, n), make([]string, n)
		reqCut, resCut = make([]bool, n), make([]bool, n)
	)
	for i, l := range logs {
		ids[i], ws[i], keys[i] = l.ID, l.WorkspaceID, l.KeyID
		route[i], vendor[i], model[i], outcome[i] = l.RouteAlias, l.Vendor, l.Model, string(l.Outcome)
		status[i], created[i] = int32(l.Status), l.CreatedAt
		request[i], resp[i], reqCut[i], resCut[i] = l.Request, l.Response, l.RequestTruncated, l.ResponseTruncated
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO request_logs (id, workspace_id, key_id, route_alias, vendor, model, status, outcome,
		                           created_at, request, response, request_truncated, response_truncated)
		 SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::text[], $5::text[], $6::text[], $7::int[],
		                      $8::text[], $9::timestamptz[], $10::text[], $11::text[], $12::boolean[], $13::boolean[])
		 ON CONFLICT (id) DO NOTHING`,
		ids, ws, keys, route, vendor, model, status, outcome, created, request, resp, reqCut, resCut); err != nil {
		return fmt.Errorf("write request logs: %w", err)
	}
	return nil
}

// RequestLogs returns a workspace's logs newest first, ordered by created_at
// and then id so that a page boundary never skips or repeats a log.
func (s *Store) RequestLogs(ctx context.Context, workspaceID string, q LogQuery) ([]RequestLog, error) {
	id, err := parseID(workspaceID)
	if err != nil {
		return nil, nil
	}
	var from, to, afterAt *time.Time
	var afterID *string
	if !q.From.IsZero() {
		from = &q.From
	}
	if !q.To.IsZero() {
		to = &q.To
	}
	if q.After != nil {
		afterAt, afterID = &q.After.CreatedAt, &q.After.ID
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, key_id, route_alias, vendor, model, status, outcome, created_at,
		        request, response, request_truncated, response_truncated
		   FROM request_logs
		  WHERE workspace_id = $1
		    AND ($2 = '' OR route_alias = $2)
		    AND ($3::timestamptz IS NULL OR created_at >= $3)
		    AND ($4::timestamptz IS NULL OR created_at < $4)
		    AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6::uuid))
		  ORDER BY created_at DESC, id DESC
		  LIMIT $7`,
		id, q.Route, from, to, afterAt, afterID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("query request logs: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RequestLog, error) {
		var l RequestLog
		err := row.Scan(&l.ID, &l.WorkspaceID, &l.KeyID, &l.RouteAlias, &l.Vendor, &l.Model, &l.Status, &l.Outcome,
			&l.CreatedAt, &l.Request, &l.Response, &l.RequestTruncated, &l.ResponseTruncated)
		return l, err
	})
}

// DeleteRequestLogs deletes every log of a workspace and says how many.
func (s *Store) DeleteRequestLogs(ctx context.Context, workspaceID string) (int64, error) {
	id, err := parseID(workspaceID)
	if err != nil {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM request_logs WHERE workspace_id = $1`, id)
	if err != nil {
		return 0, fmt.Errorf("delete request logs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneRequestLogs deletes the logs each workspace's retention has passed as
// of now, batch rows at a time, and returns how many it deleted. A workspace with logging off is pruned by
// the same retention.
func (s *Store) PruneRequestLogs(ctx context.Context, now time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM request_logs
			  WHERE id IN (
			      SELECT l.id
			        FROM request_logs l
			        JOIN workspaces w ON w.id = l.workspace_id
			       WHERE l.created_at < $1::timestamptz - make_interval(days =>
			                 coalesce(nullif((w.settings->>'prompt_log_retention_days')::int, 0), $3))
			       LIMIT $2)`,
			now, batch, DefaultPromptLogRetentionDays)
		if err != nil {
			return total, fmt.Errorf("prune request logs: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}
