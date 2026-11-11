package pgxtel

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"maps"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
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
	checkPoolPoints(t, reader, registration)
}

// TestObservePgxPool pins the same metrics for a pgxpool.Pool.
func TestObservePgxPool(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	config, err := pgxpool.ParseConfig("postgres://app@" + fakeServer(t) + "/ledger?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 7
	tracer, err := New(config.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Two connections stay in use, a third returns idle.
	for i := range 3 {
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			conn.Release()
		} else {
			t.Cleanup(conn.Release)
		}
	}
	registration, err := tracer.ObservePgxPool(pool, "main")
	if err != nil {
		t.Fatal(err)
	}
	checkPoolPoints(t, reader, registration)
}

// checkPoolPoints expects the points of a pool of seven with two connections in use and one idle, and
// none once registration is unregistered.
func checkPoolPoints(t *testing.T, reader sdkmetric.Reader, registration metric.Registration) {
	t.Helper()
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

// fakeServer listens as a PostgreSQL server that accepts every connection and then only waits for it
// to close, which is all a pool needs to hold connections; it returns the address.
func fakeServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(&pgproto3.AuthenticationOk{})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if err := backend.Flush(); err != nil {
					return
				}
				for {
					if _, err := backend.Receive(); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
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
