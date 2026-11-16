package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// FreeQuota is one free_quota_catalog row: what a vendor's free tier allows
// one key on one model. A nil limit is one the vendor does not publish.
// PerAccount says the key's per-account models share the limits, as
// OpenRouter's :free models share 50 requests a day.
type FreeQuota struct {
	Vendor, Model      string
	RPM, RPD, TPM, TPD *int
	PerAccount         bool
}

// FreeQuotas returns the whole catalog. It is a few dozen rows that belong to
// no workspace.
func (s *Store) FreeQuotas(ctx context.Context) ([]FreeQuota, error) {
	rows, err := s.pool.Query(ctx, `SELECT vendor, model, rpm, rpd, tpm, tpd, per_account FROM free_quota_catalog`)
	if err != nil {
		return nil, fmt.Errorf("query free quota catalog: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (FreeQuota, error) {
		var q FreeQuota
		err := row.Scan(&q.Vendor, &q.Model, &q.RPM, &q.RPD, &q.TPM, &q.TPD, &q.PerAccount)
		return q, err
	})
}
