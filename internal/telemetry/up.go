package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// upProbeTimeout bounds one ping, so a hung dependency delays a metrics
// collection by this much and no more.
const upProbeTimeout = 2 * time.Second

// ObserveUp registers an observable gauge named "<name>.up" that reads 1 when
// ping succeeds and 0 when it fails. The ping runs inside the collection
// callback, so it costs one round trip per export interval and nothing
// between exports.
func ObserveUp(name string, ping func(context.Context) error) error {
	_, err := otel.Meter("llm-gateway/telemetry").Int64ObservableGauge(name+".up",
		metric.WithDescription("1 when the "+name+" dependency answers a ping, 0 when it does not."),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			ctx, cancel := context.WithTimeout(ctx, upProbeTimeout)
			defer cancel()
			if ping(ctx) != nil {
				o.Observe(0)
				return nil
			}
			o.Observe(1)
			return nil
		}),
	)
	return err
}
