package pgxtel

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/dbconv"
)

// ObservePool reports db's connections in use and idle as db.client.connection.count, and its
// connection limit as db.client.connection.max, as the pool named name, at each metric collection
// until the returned registration is unregistered.
func (t *Tracer) ObservePool(db *sql.DB, name string) (metric.Registration, error) {
	return t.observePool(name, func() poolStats {
		stats := db.Stats()
		return poolStats{used: int64(stats.InUse), idle: int64(stats.Idle), limit: int64(stats.MaxOpenConnections)}
	})
}

// ObservePgxPool reports a pgxpool.Pool as ObservePool reports a database/sql one.
func (t *Tracer) ObservePgxPool(pool *pgxpool.Pool, name string) (metric.Registration, error) {
	return t.observePool(name, func() poolStats {
		stats := pool.Stat()
		return poolStats{used: int64(stats.AcquiredConns()), idle: int64(stats.IdleConns()), limit: int64(stats.MaxConns())}
	})
}

// poolStats is one reading of a connection pool.
type poolStats struct {
	used, idle, limit int64
}

// observePool reports the pool read reads, named name, at each metric collection.
func (t *Tracer) observePool(name string, read func() poolStats) (metric.Registration, error) {
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
		stats := read()
		o.ObserveInt64(connections.Inst(), stats.used, used)
		o.ObserveInt64(connections.Inst(), stats.idle, idle)
		o.ObserveInt64(limit.Inst(), stats.limit, pool)
		return nil
	}, connections.Inst(), limit.Inst())
	if err != nil {
		return nil, fmt.Errorf("observe SQL connection pool: %w", err)
	}
	return registration, nil
}
