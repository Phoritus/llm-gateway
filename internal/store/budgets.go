package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// BudgetAction is what a spent budget does: refuse requests, or only mail
// the workspace owner.
type BudgetAction string

const (
	BudgetBlock BudgetAction = "block"
	BudgetWarn  BudgetAction = "warn"
)

// Budget is a monthly cap in micro-USD. A nil *Budget is no cap; zero is a
// cap of nothing.
type Budget struct {
	MicroUSD int64        `json:"micro_usd"`
	Action   BudgetAction `json:"action"`
}

// ModelPrice is one model_prices row, each price in pico-USD per token
// (USD per million tokens times a million), so cost is integer arithmetic. A
// nil CachedInput is a vendor that prices no cache reads.
type ModelPrice struct {
	Vendor, Model string
	Input, Output int64
	CachedInput   *int64
}

// ModelPrices returns the whole price table.
func (s *Store) ModelPrices(ctx context.Context) ([]ModelPrice, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT vendor, model, (input_usd_per_mtok * 1000000)::bigint, (output_usd_per_mtok * 1000000)::bigint,
		        (cached_input_usd_per_mtok * 1000000)::bigint
		   FROM model_prices`)
	if err != nil {
		return nil, fmt.Errorf("query model prices: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ModelPrice, error) {
		var p ModelPrice
		err := row.Scan(&p.Vendor, &p.Model, &p.Input, &p.Output, &p.CachedInput)
		return p, err
	})
}

// BudgetContact is who a budget mail goes to, and, for a key's budget, the
// key's name. keyID may be empty.
func (s *Store) BudgetContact(ctx context.Context, workspaceID, keyID string) (email, keyName string, err error) {
	ws, err := parseID(workspaceID)
	if err != nil {
		return "", "", err
	}
	var name *string
	err = s.pool.QueryRow(ctx,
		`SELECT u.email,
		        (SELECT k.name FROM gateway_keys k WHERE k.workspace_id = w.id AND k.id::text = $2)
		   FROM workspaces w JOIN users u ON u.id = w.owner_id
		  WHERE w.id = $1`, ws, keyID).Scan(&email, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("select budget contact: %w", err)
	}
	if name != nil {
		keyName = *name
	}
	return email, keyName, nil
}

// SetGatewayKeyBudget replaces an unrevoked key's budget; nil removes it.
// ErrNotFound covers a key that is absent, revoked, or another workspace's.
func (s *Store) SetGatewayKeyBudget(ctx context.Context, workspaceID, id string, b *Budget) (*GatewayKey, error) {
	id, err := parseID(id)
	if err != nil {
		return nil, err
	}
	amount, action := budgetColumns(b)
	k, err := scanGatewayKey(s.pool.QueryRow(ctx,
		`UPDATE gateway_keys SET budget_micro_usd = $3, budget_action = $4
		  WHERE workspace_id = $1 AND id = $2 AND revoked_at IS NULL
		 RETURNING `+gatewayKeyColumns, workspaceID, id, amount, action))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update gateway key budget: %w", err)
	}
	return &k, nil
}

func budgetColumns(b *Budget) (*int64, *string) {
	if b == nil {
		return nil, nil
	}
	action := string(b.Action)
	return &b.MicroUSD, &action
}

func budgetOf(amount *int64, action *string) *Budget {
	if amount == nil || action == nil {
		return nil
	}
	return &Budget{MicroUSD: *amount, Action: BudgetAction(*action)}
}
