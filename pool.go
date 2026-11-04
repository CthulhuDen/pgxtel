package pgxtel

import (
	"context"
	"database/sql"
	"fmt"

	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/dbconv"
)

// ObservePool reports db's connections in use and idle as db.client.connection.count, and its
// connection limit as db.client.connection.max, as the pool named name, at each metric collection
// until the returned registration is unregistered.
func (t *Tracer) ObservePool(db *sql.DB, name string) (metric.Registration, error) {
	connections, err := dbconv.NewClientConnectionCountObservable(t.meter)
	if err != nil {
		return nil, fmt.Errorf("create SQL connection count metric: %w", err)
	}
	limit, err := dbconv.NewClientConnectionMaxObservable(t.meter)
	if err != nil {
		return nil, fmt.Errorf("create SQL connection limit metric: %w", err)
	}
	poolName := semconv.DBClientConnectionPoolName(name)
	pool := metric.WithAttributes(poolName)
	used := metric.WithAttributes(poolName, semconv.DBClientConnectionStateUsed)
	idle := metric.WithAttributes(poolName, semconv.DBClientConnectionStateIdle)
	registration, err := t.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		stats := db.Stats()
		o.ObserveInt64(connections.Inst(), int64(stats.InUse), used)
		o.ObserveInt64(connections.Inst(), int64(stats.Idle), idle)
		o.ObserveInt64(limit.Inst(), int64(stats.MaxOpenConnections), pool)
		return nil
	}, connections.Inst(), limit.Inst())
	if err != nil {
		return nil, fmt.Errorf("observe SQL connection pool: %w", err)
	}
	return registration, nil
}
