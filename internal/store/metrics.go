package store

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// RegisterMetrics exposes the connection pool's occupancy. The readings are
// taken from pgxpool.Stat when the exporter collects, so they cost nothing
// per query. Pool exhaustion is the first thing a free-tier connection limit
// shows up as, which is why they are worth a gauge.
func (s *Store) RegisterMetrics() error {
	meter := otel.Meter("llm-gateway/store")

	acquired, err := meter.Int64ObservableGauge("postgres.pool.acquired",
		metric.WithDescription("Connections currently checked out of the pool."))
	if err != nil {
		return err
	}
	idle, err := meter.Int64ObservableGauge("postgres.pool.idle",
		metric.WithDescription("Connections open and waiting in the pool."))
	if err != nil {
		return err
	}
	total, err := meter.Int64ObservableGauge("postgres.pool.total",
		metric.WithDescription("Connections open, acquired plus idle plus being opened."))
	if err != nil {
		return err
	}
	maxConns, err := meter.Int64ObservableGauge("postgres.pool.max",
		metric.WithDescription("Configured ceiling on open connections."))
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := s.pool.Stat()
		o.ObserveInt64(acquired, int64(st.AcquiredConns()))
		o.ObserveInt64(idle, int64(st.IdleConns()))
		o.ObserveInt64(total, int64(st.TotalConns()))
		o.ObserveInt64(maxConns, int64(st.MaxConns()))
		return nil
	}, acquired, idle, total, maxConns)
	return err
}
