package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outcome is how a request, or one attempt at a target, ended. It is a stored
// column, a header value and a metric label, so it is one of the constants
// declared here, in router and in gateway, and never text from an upstream.
type Outcome string

const (
	OutcomeOK          Outcome = "ok"
	OutcomeStreamError Outcome = "stream_error"
	OutcomeClientGone  Outcome = "client_gone"
)

// notErrors are the outcomes the rollup leaves out of its error count. A
// client that hung up is not the service failing.
var notErrors = []string{string(OutcomeOK), string(OutcomeClientGone)}

// UsageEvent is one request that resolved a route.
type UsageEvent struct {
	// ID is a UUID minted once by whoever records the event. WriteBatch stores
	// an ID once however often it is handed it, which is what makes a retry
	// safe.
	ID          string
	WorkspaceID string
	KeyID       string
	RouteAlias  string
	// Vendor and Model name the target that answered, and are empty when none did.
	Vendor string
	Model  string
	// Status is what the client was told. A stream that failed after its first
	// chunk is still 200: Outcome says it failed.
	Status           int
	Outcome          Outcome
	Attempts         int
	PromptTokens     int
	CompletionTokens int
	LatencyMs        int
	// TTFTMs is zero when no chunk arrived, and when the first one took under
	// a millisecond.
	TTFTMs    int
	Cached    bool
	CreatedAt time.Time
}

// UsageDay is one day's totals for a route, vendor and model.
type UsageDay struct {
	Day              time.Time
	RouteAlias       string
	Vendor           string
	Model            string
	Requests         int64
	Errors           int64
	PromptTokens     int64
	CompletionTokens int64
	LatencyMsSum     int64
}

// WriteBatch stores events and adds them to the daily rollup in one
// statement. A batch that fails counts nothing. An event whose ID is already
// stored is skipped and adds nothing to the rollup, so a batch retried after
// its acknowledgement was lost counts once.
func (s *Store) WriteBatch(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	n := len(events)
	var (
		ids, ws, keys, route, vendor, model, outcome = make([]string, n), make([]string, n), make([]string, n),
			make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		status, attempts                = make([]int32, n), make([]int32, n)
		prompt, completion, latency, tt = make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
		cached                          = make([]bool, n)
		created                         = make([]time.Time, n)
	)
	for i, e := range events {
		ids[i], ws[i], keys[i] = e.ID, e.WorkspaceID, e.KeyID
		route[i], vendor[i], model[i], outcome[i] = e.RouteAlias, e.Vendor, e.Model, string(e.Outcome)
		status[i], attempts[i] = int32(e.Status), int32(e.Attempts)
		prompt[i], completion[i] = int64(e.PromptTokens), int64(e.CompletionTokens)
		latency[i], tt[i] = int64(e.LatencyMs), int64(e.TTFTMs)
		cached[i], created[i] = e.Cached, e.CreatedAt
	}

	// The rollup is built from the rows the insert returns, not from the
	// batch, so it counts exactly what was stored. It is ordered so that two
	// instances upserting overlapping rows take the row locks in the same
	// order and cannot deadlock.
	if _, err := s.pool.Exec(ctx,
		`WITH stored AS (
		     INSERT INTO usage_events (id, workspace_id, key_id, route_alias, vendor, model, status, outcome,
		                               attempts, prompt_tokens, completion_tokens, latency_ms, ttft_ms, cached, created_at)
		     SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::text[], $5::text[], $6::text[], $7::int[],
		                          $8::text[], $9::int[], $10::bigint[], $11::bigint[], $12::bigint[], $13::bigint[],
		                          $14::boolean[], $15::timestamptz[])
		     ON CONFLICT (id) DO NOTHING
		     RETURNING workspace_id, created_at, route_alias, vendor, model, outcome,
		               prompt_tokens, completion_tokens, latency_ms
		 )
		 INSERT INTO usage_daily (workspace_id, day, route_alias, vendor, model,
		                          requests, errors, prompt_tokens, completion_tokens, latency_ms_sum)
		 SELECT workspace_id, (created_at AT TIME ZONE 'UTC')::date, route_alias, vendor, model,
		        count(*), count(*) FILTER (WHERE outcome <> ALL($16::text[])),
		        sum(prompt_tokens), sum(completion_tokens), sum(latency_ms)
		   FROM stored
		  GROUP BY 1, 2, 3, 4, 5
		  ORDER BY 1, 2, 3, 4, 5
		 ON CONFLICT (workspace_id, day, route_alias, vendor, model) DO UPDATE SET
		     requests          = usage_daily.requests + EXCLUDED.requests,
		     errors            = usage_daily.errors + EXCLUDED.errors,
		     prompt_tokens     = usage_daily.prompt_tokens + EXCLUDED.prompt_tokens,
		     completion_tokens = usage_daily.completion_tokens + EXCLUDED.completion_tokens,
		     latency_ms_sum    = usage_daily.latency_ms_sum + EXCLUDED.latency_ms_sum`,
		ids, ws, keys, route, vendor, model, status, outcome, attempts, prompt, completion, latency, tt,
		cached, created, notErrors); err != nil {
		return fmt.Errorf("write usage batch: %w", err)
	}
	return nil
}

// Daily returns a workspace's rollup rows for the days from through to,
// inclusive, oldest first. A non-empty routeAlias narrows it to that route.
func (s *Store) Daily(ctx context.Context, workspaceID string, from, to time.Time, routeAlias string) ([]UsageDay, error) {
	id, err := parseID(workspaceID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT day, route_alias, vendor, model, requests, errors, prompt_tokens, completion_tokens, latency_ms_sum
		   FROM usage_daily
		  WHERE workspace_id = $1 AND day BETWEEN $2::date AND $3::date
		    AND ($4 = '' OR route_alias = $4)
		  ORDER BY day, route_alias, vendor, model`,
		id, from, to, routeAlias)
	if err != nil {
		return nil, fmt.Errorf("query usage rollup: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageDay, error) {
		var d UsageDay
		err := row.Scan(&d.Day, &d.RouteAlias, &d.Vendor, &d.Model, &d.Requests, &d.Errors,
			&d.PromptTokens, &d.CompletionTokens, &d.LatencyMsSum)
		return d, err
	})
}

// Recent returns a workspace's latest events, newest first.
func (s *Store) Recent(ctx context.Context, workspaceID string, limit int) ([]UsageEvent, error) {
	id, err := parseID(workspaceID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT workspace_id, key_id, route_alias, vendor, model, status, outcome, attempts,
		        prompt_tokens, completion_tokens, latency_ms, ttft_ms, cached, created_at
		   FROM usage_events
		  WHERE workspace_id = $1
		  ORDER BY created_at DESC, id DESC
		  LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent usage: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageEvent, error) {
		var e UsageEvent
		err := row.Scan(&e.WorkspaceID, &e.KeyID, &e.RouteAlias, &e.Vendor, &e.Model, &e.Status, &e.Outcome,
			&e.Attempts, &e.PromptTokens, &e.CompletionTokens, &e.LatencyMs, &e.TTFTMs, &e.Cached, &e.CreatedAt)
		return e, err
	})
}

// PruneEvents deletes raw events older than before, batch rows at a time so
// that no one statement holds a long lock, and returns how many it deleted.
// The rollup is never pruned.
func (s *Store) PruneEvents(ctx context.Context, before time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM usage_events
			  WHERE id IN (SELECT id FROM usage_events WHERE created_at < $1 LIMIT $2)`, before, batch)
		if err != nil {
			return total, fmt.Errorf("prune usage events: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}
