package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// FreeQuota is one free_quota_catalog row: what a vendor's free tier allows
// one key on one model. A nil limit is one the vendor does not publish.
type FreeQuota struct {
	Vendor, Model      string
	RPM, RPD, TPM, TPD *int
}

// FreeQuotas returns the whole catalog. It is a few dozen rows that belong to
// no workspace.
func (s *Store) FreeQuotas(ctx context.Context) ([]FreeQuota, error) {
	rows, err := s.pool.Query(ctx, `SELECT vendor, model, rpm, rpd, tpm, tpd FROM free_quota_catalog`)
	if err != nil {
		return nil, fmt.Errorf("query free quota catalog: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (FreeQuota, error) {
		var q FreeQuota
		err := row.Scan(&q.Vendor, &q.Model, &q.RPM, &q.RPD, &q.TPM, &q.TPD)
		return q, err
	})
}
