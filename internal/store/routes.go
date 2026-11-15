package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxRoutesPerWorkspace = 100
	foreignKeyViolation   = "23503"
)

var (
	// ErrAliasTaken means the workspace already has a route with that alias.
	ErrAliasTaken = errors.New("store: route alias already in use")
	// ErrUnknownCredential means a target names a credential the workspace
	// does not have, whether it never existed or belongs to another tenant.
	ErrUnknownCredential = errors.New("store: target names an unknown credential")
	// ErrCredentialInUse means a route still names the credential.
	ErrCredentialInUse = errors.New("store: credential is used by a route")
)

// CacheMode is a route's exact-cache policy.
type CacheMode string

const (
	// CacheAuto caches only a request whose temperature is exactly 0.
	CacheAuto CacheMode = "auto"
	// CacheAlways caches every cacheable request on the route.
	CacheAlways CacheMode = "always"
	CacheOff    CacheMode = "off"
)

// ParseCacheMode accepts only the exact lowercase name.
func ParseCacheMode(s string) (CacheMode, bool) {
	switch m := CacheMode(s); m {
	case CacheAuto, CacheAlways, CacheOff:
		return m, true
	}
	return "", false
}

type Route struct {
	ID          string
	WorkspaceID string
	Alias       string
	FreeFirst   bool
	Cache       CacheMode
	MaxHops     int
	Targets     []RouteTarget
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RouteTarget is one entry in a route's chain. Its position is its index in
// Route.Targets.
type RouteTarget struct {
	CredentialID string
	Model        string
}

type RouteSpec struct {
	Alias     string
	FreeFirst bool
	Cache     CacheMode
	MaxHops   int
	Targets   []RouteTarget
}

const routeColumns = `id, workspace_id, alias, free_first, cache, max_hops, created_at, updated_at`

func scanRoute(row pgx.Row) (Route, error) {
	var r Route
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.Alias, &r.FreeFirst, &r.Cache, &r.MaxHops, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// parseID canonicalizes a caller-supplied row id. uuid.Parse accepts forms
// Postgres does not, such as urn:uuid:, so the raw string must not reach SQL.
func parseID(id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", ErrNotFound
	}
	return u.String(), nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// CreateRoute stores a route and its targets. It returns ErrLimitReached,
// ErrAliasTaken or ErrUnknownCredential when the spec cannot be stored.
func (s *Store) CreateRoute(ctx context.Context, workspaceID string, in RouteSpec) (*Route, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create route: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockWorkspaceForCap(ctx, tx, workspaceID); err != nil {
		return nil, err
	}

	r, err := scanRoute(tx.QueryRow(ctx,
		`INSERT INTO routes (id, workspace_id, alias, free_first, cache, max_hops)
		 SELECT $1, $2, $3, $4, $5, $6
		  WHERE (SELECT count(*) FROM routes WHERE workspace_id = $2) < $7
		 RETURNING `+routeColumns,
		uuid.NewString(), workspaceID, in.Alias, in.FreeFirst, in.Cache, in.MaxHops, maxRoutesPerWorkspace))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrLimitReached
	case pgCode(err) == uniqueViolation:
		return nil, ErrAliasTaken
	case err != nil:
		return nil, fmt.Errorf("insert route: %w", err)
	}
	if err := insertTargets(ctx, tx, r.ID, workspaceID, in.Targets); err != nil {
		return nil, err
	}
	if err := commitRoute(ctx, tx); err != nil {
		return nil, err
	}
	r.Targets = in.Targets
	return &r, nil
}

// UpdateRoute replaces a route's settings and its whole chain of targets.
func (s *Store) UpdateRoute(ctx context.Context, workspaceID, id string, in RouteSpec) (*Route, error) {
	id, err := parseID(id)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin update route: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Credentials.Delete takes the same lock first. Without it, replacing the
	// targets and deleting a credential they name lock rows in opposite orders
	// and deadlock.
	if err := lockWorkspaceForCap(ctx, tx, workspaceID); err != nil {
		return nil, err
	}

	r, err := scanRoute(tx.QueryRow(ctx,
		`UPDATE routes SET alias = $3, free_first = $4, cache = $5, max_hops = $6, updated_at = now()
		  WHERE workspace_id = $1 AND id = $2
		 RETURNING `+routeColumns,
		workspaceID, id, in.Alias, in.FreeFirst, in.Cache, in.MaxHops))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNotFound
	case pgCode(err) == uniqueViolation:
		return nil, ErrAliasTaken
	case err != nil:
		return nil, fmt.Errorf("update route: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM route_targets WHERE workspace_id = $1 AND route_id = $2`, workspaceID, id); err != nil {
		return nil, fmt.Errorf("clear route targets: %w", err)
	}
	if err := insertTargets(ctx, tx, id, workspaceID, in.Targets); err != nil {
		return nil, err
	}
	if err := commitRoute(ctx, tx); err != nil {
		return nil, err
	}
	r.Targets = in.Targets
	return &r, nil
}

// commitRoute commits a route write. The credential foreign key is deferred, so
// a credential deleted after the targets were inserted surfaces here.
func commitRoute(ctx context.Context, tx pgx.Tx) error {
	err := tx.Commit(ctx)
	if pgCode(err) == foreignKeyViolation {
		return ErrUnknownCredential
	}
	if err != nil {
		return fmt.Errorf("commit route: %w", err)
	}
	return nil
}

func insertTargets(ctx context.Context, tx pgx.Tx, routeID, workspaceID string, targets []RouteTarget) error {
	creds := make([]string, len(targets))
	models := make([]string, len(targets))
	for i, t := range targets {
		creds[i], models[i] = t.CredentialID, t.Model
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO route_targets (route_id, workspace_id, position, credential_id, model)
		 SELECT $1, $2, t.ord - 1, t.cred, t.model
		   FROM unnest($3::uuid[], $4::text[]) WITH ORDINALITY AS t(cred, model, ord)`,
		routeID, workspaceID, creds, models)
	if pgCode(err) == foreignKeyViolation {
		return ErrUnknownCredential
	}
	if err != nil {
		return fmt.Errorf("insert route targets: %w", err)
	}
	return nil
}

// ListRoutes returns the workspace's routes with their targets, ordered by
// alias.
func (s *Store) ListRoutes(ctx context.Context, workspaceID string) ([]Route, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin list routes: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT `+routeColumns+` FROM routes WHERE workspace_id = $1 ORDER BY alias`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	routes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Route, error) { return scanRoute(row) })
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}

	index := make(map[string]*Route, len(routes))
	ids := make([]string, len(routes))
	for i := range routes {
		index[routes[i].ID] = &routes[i]
		ids[i] = routes[i].ID
	}
	rows, err = tx.Query(ctx,
		`SELECT route_id, credential_id, model FROM route_targets
		  WHERE workspace_id = $1 AND route_id = ANY($2::uuid[]) ORDER BY route_id, position`, workspaceID, ids)
	if err != nil {
		return nil, fmt.Errorf("list route targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var routeID string
		var t RouteTarget
		if err := rows.Scan(&routeID, &t.CredentialID, &t.Model); err != nil {
			return nil, fmt.Errorf("scan route target: %w", err)
		}
		if r := index[routeID]; r != nil {
			r.Targets = append(r.Targets, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list route targets: %w", err)
	}
	return routes, nil
}

// DeleteRoute removes a route and its targets, or returns ErrNotFound when the
// workspace has no such route.
func (s *Store) DeleteRoute(ctx context.Context, workspaceID, id string) error {
	id, err := parseID(id)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM routes WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete route: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
