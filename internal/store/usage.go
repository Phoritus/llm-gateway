package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The outcomes the store itself has to tell apart. The rest of the fixed set
// (the reduced exhaustion kinds) is only ever stored and shown.
const (
	OutcomeOK          = "ok"
	OutcomeStreamError = "stream_error"
	OutcomeClientGone  = "client_gone"
)

// UsageEvent is one request that resolved a route.
type UsageEvent struct {
	WorkspaceID string
	KeyID       string
	RouteAlias  string
	// Vendor and Model name the target that answered, and are empty when none did.
	Vendor string
	Model  string
	// Status is what the client was told. A stream that failed after its first
	// chunk is still 200: Outcome says it failed.
	Status           int
	Outcome          string
	Attempts         int
	PromptTokens     int
	CompletionTokens int
	LatencyMs        int
	// TTFTMs is zero when no chunk arrived.
	TTFTMs    int
	Cached    bool
	CreatedAt time.Time
}

// Failed reports whether the event counts as an error in the rollup. A client
// that hung up is not the service failing.
func (e UsageEvent) Failed() bool {
	return e.Outcome != OutcomeOK && e.Outcome != OutcomeClientGone
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

// dayKey identifies one rollup row.
type dayKey struct {
	workspaceID string
	day         time.Time
	route       string
	vendor      string
	model       string
}

func (a dayKey) compare(b dayKey) int {
	for _, c := range []int{
		strings.Compare(a.workspaceID, b.workspaceID),
		a.day.Compare(b.day),
		strings.Compare(a.route, b.route),
		strings.Compare(a.vendor, b.vendor),
		strings.Compare(a.model, b.model),
	} {
		if c != 0 {
			return c
		}
	}
	return 0
}

// WriteBatch stores events and adds them to the daily rollup in one
// transaction. A batch that fails counts nothing, and a retried one counts
// once.
func (s *Store) WriteBatch(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin usage batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows := make([][]any, len(events))
	totals := map[dayKey]*UsageDay{}
	for i, e := range events {
		rows[i] = []any{e.WorkspaceID, e.KeyID, e.RouteAlias, e.Vendor, e.Model, e.Status, e.Outcome,
			e.Attempts, e.PromptTokens, e.CompletionTokens, e.LatencyMs, e.TTFTMs, e.Cached, e.CreatedAt}

		day := e.CreatedAt.UTC().Truncate(24 * time.Hour)
		k := dayKey{e.WorkspaceID, day, e.RouteAlias, e.Vendor, e.Model}
		t := totals[k]
		if t == nil {
			t = &UsageDay{}
			totals[k] = t
		}
		t.Requests++
		if e.Failed() {
			t.Errors++
		}
		t.PromptTokens += int64(e.PromptTokens)
		t.CompletionTokens += int64(e.CompletionTokens)
		t.LatencyMsSum += int64(e.LatencyMs)
	}

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"usage_events"},
		[]string{"workspace_id", "key_id", "route_alias", "vendor", "model", "status", "outcome", "attempts",
			"prompt_tokens", "completion_tokens", "latency_ms", "ttft_ms", "cached", "created_at"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("insert usage events: %w", err)
	}

	// Sorted so that two instances upserting overlapping rows take the row
	// locks in the same order and cannot deadlock.
	keys := make([]dayKey, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, dayKey.compare)

	n := len(keys)
	var (
		ws, route, vendor, model []string
		days                     []time.Time
		requests                 = make([]int64, 0, n)
		errs                     = make([]int64, 0, n)
		prompt                   = make([]int64, 0, n)
		completion               = make([]int64, 0, n)
		latency                  = make([]int64, 0, n)
	)
	for _, k := range keys {
		t := totals[k]
		ws, days = append(ws, k.workspaceID), append(days, k.day)
		route, vendor, model = append(route, k.route), append(vendor, k.vendor), append(model, k.model)
		requests, errs = append(requests, t.Requests), append(errs, t.Errors)
		prompt, completion = append(prompt, t.PromptTokens), append(completion, t.CompletionTokens)
		latency = append(latency, t.LatencyMsSum)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO usage_daily (workspace_id, day, route_alias, vendor, model,
		                          requests, errors, prompt_tokens, completion_tokens, latency_ms_sum)
		 SELECT * FROM unnest($1::uuid[], $2::date[], $3::text[], $4::text[], $5::text[],
		                      $6::bigint[], $7::bigint[], $8::bigint[], $9::bigint[], $10::bigint[])
		 ON CONFLICT (workspace_id, day, route_alias, vendor, model) DO UPDATE SET
		     requests          = usage_daily.requests + EXCLUDED.requests,
		     errors            = usage_daily.errors + EXCLUDED.errors,
		     prompt_tokens     = usage_daily.prompt_tokens + EXCLUDED.prompt_tokens,
		     completion_tokens = usage_daily.completion_tokens + EXCLUDED.completion_tokens,
		     latency_ms_sum    = usage_daily.latency_ms_sum + EXCLUDED.latency_ms_sum`,
		ws, days, route, vendor, model, requests, errs, prompt, completion, latency); err != nil {
		return fmt.Errorf("upsert usage rollup: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit usage batch: %w", err)
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
