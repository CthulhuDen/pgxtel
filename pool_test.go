package pgxtel

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestObservePool pins the connection pool metrics, which degrade silently: the pool's connections in
// use and idle and its limit, each under the pool's name, until the registration is unregistered.
func TestObservePool(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	config, err := pgx.ParseConfig("postgres://app@db:5433/ledger")
	if err != nil {
		t.Fatal(err)
	}
	tracer, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(fakeConnector{})
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(7)
	// Two connections stay in use, a third returns idle.
	for i := range 3 {
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			_ = conn.Close()
		} else {
			t.Cleanup(func() { _ = conn.Close() })
		}
	}
	registration, err := tracer.ObservePool(db, "main")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]int64{"db.client.connection.count used": 2, "db.client.connection.count idle": 1,
		"db.client.connection.max": 7}
	if got := poolPoints(t, reader); !maps.Equal(got, want) {
		t.Errorf("pool points = %v, want %v", got, want)
	}
	if err := registration.Unregister(); err != nil {
		t.Fatal(err)
	}
	if got := poolPoints(t, reader); len(got) != 0 {
		t.Errorf("pool points after unregistering = %v, want none", got)
	}
}

// poolPoints returns the collected connection pool points of the pool named main, keyed by metric
// name and connection state.
func poolPoints(t *testing.T, reader sdkmetric.Reader) map[string]int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	points := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				if pool, _ := point.Attributes.Value("db.client.connection.pool.name"); pool.AsString() != "main" {
					continue
				}
				key := m.Name
				if state, ok := point.Attributes.Value("db.client.connection.state"); ok {
					key += " " + state.AsString()
				}
				points[key] = point.Value
			}
		}
	}
	return points
}

// fakeConnector opens connections that database/sql can pool but never uses.
type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) { return fakeConn{}, nil }
func (fakeConnector) Driver() driver.Driver                        { return nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
