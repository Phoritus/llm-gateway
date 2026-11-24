package keycheck

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

const Timeout = 10 * time.Second

func Result(err error) store.KeyCheckResult {
	switch {
	case err == nil:
		return store.KeyCheckOK
	case errors.Is(err, provider.ErrAuth), errors.Is(err, provider.ErrModelUnavailable):
		return store.KeyCheckRejected
	case errors.Is(err, provider.ErrQuota):
		return store.KeyCheckQuotaExhausted
	case errors.Is(err, provider.ErrRateLimited):
		return store.KeyCheckRateLimited
	case errors.Is(err, provider.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return store.KeyCheckProviderTimeout
	default:
		return store.KeyCheckProviderError
	}
}

func Check(ctx context.Context, providers map[provider.Vendor]provider.Provider, cred store.UnsealedCredential) (store.KeyCheckResult, error) {
	adapter, ok := providers[cred.Vendor]
	if !ok {
		return store.KeyCheckProviderUnavailable, nil
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	err := adapter.CheckKey(ctx, provider.Credential{APIKey: cred.Secret, BaseURL: cred.BaseURL})
	return Result(err), err
}

type Store interface {
	ClaimDueChecks(ctx context.Context, interval time.Duration, limit int) ([]store.DueCheck, error)
	Unseal(ctx context.Context, workspaceID, id string) (store.UnsealedCredential, error)
	RecordCheck(ctx context.Context, workspaceID, id string, check store.KeyCheck) error
}

type Checker struct {
	Store     Store
	Providers map[provider.Vendor]provider.Provider
	Interval  time.Duration
	PerTick   int
	Logger    *slog.Logger
}

func (c *Checker) Run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if _, err := c.CheckDue(ctx); err != nil && ctx.Err() == nil {
			c.logger().Error("key checks failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Checker) CheckDue(ctx context.Context) (int, error) {
	due, err := c.Store.ClaimDueChecks(ctx, c.Interval, c.PerTick)
	if err != nil {
		return 0, err
	}
	checked := 0
	for _, d := range due {
		cred, err := c.Store.Unseal(ctx, d.WorkspaceID, d.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return checked, err
		}
		result, checkErr := Check(ctx, c.Providers, cred)
		if ctx.Err() != nil {
			return checked, ctx.Err()
		}
		if checkErr != nil && result != store.KeyCheckRejected {
			c.logger().Info("key check failed", slog.String("vendor", string(cred.Vendor)),
				slog.String("result", string(result)), slog.String("error", checkErr.Error()))
		}
		err = c.Store.RecordCheck(ctx, d.WorkspaceID, d.ID, store.KeyCheck{Result: result, At: time.Now()})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return checked, err
		}
		checked++
	}
	return checked, nil
}

func (c *Checker) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}
