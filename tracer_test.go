package pgxtel

import (
	"context"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	lockSQL   = "/* lock targets */ SELECT id FROM items FOR UPDATE"
	updateSQL = "/* tag items */ UPDATE items SET tags = $1"
)

// TestTracer pins the spans and statement metric a Tracer records, which degrade silently: the
// describer's name and attributes, its span-only attributes on the spans alone, the defaults it
// leaves in place, failure classification, a preparation measuring nothing of its own, and the row
// counts on the span alone.
func TestTracer(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(traces)
	// Delta temporality hands each collection the statements since the previous one alone.
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(
		func(sdkmetric.InstrumentKind) metricdata.Temporality { return metricdata.DeltaTemporality }))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	config, err := pgx.ParseConfig("postgres://app@db:5433/ledger")
	if err != nil {
		t.Fatal(err)
	}
	tracer, err := New(config, WithDescriber(func(sql string, s *Statement) {
		CommentSummary(sql, s)
		s.SpanAttrs = append(s.SpanAttrs, attribute.String("test.span_only", "yes"))
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, parent := traces.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()

	tests := []struct {
		name       string
		sql        string
		prepare    bool
		err        error
		tag        string
		wantName   string
		wantStatus codes.Code
		// want is the attributes span and metric share beyond the database's own.
		want map[string]string
		// wantRows is the row counts the span alone carries.
		wantRows map[string]string
		// noMetric marks a hook that measures nothing.
		noMetric bool
	}{
		{name: "select", sql: lockSQL, tag: "SELECT 3", wantName: "lock targets",
			want:     map[string]string{"db.query.summary": "lock targets"},
			wantRows: map[string]string{"pgx.rows_affected": "3", "db.response.returned_rows": "3"}},
		{name: "write", sql: updateSQL, tag: "UPDATE 2", wantName: "tag items",
			want:     map[string]string{"db.query.summary": "tag items"},
			wantRows: map[string]string{"pgx.rows_affected": "2"}},
		{name: "server error", sql: lockSQL, err: &pgconn.PgError{Code: "40001"},
			wantName: "lock targets", wantStatus: codes.Error, want: map[string]string{
				"db.query.summary": "lock targets", "error.type": "40001", "db.response.status_code": "40001"}},
		{name: "other failure", sql: lockSQL, err: context.Canceled, wantName: "lock targets",
			wantStatus: codes.Error, want: map[string]string{"db.query.summary": "lock targets", "error.type": "_OTHER"}},
		{name: "transaction control", sql: "begin isolation level serializable", tag: "BEGIN", wantName: "BEGIN",
			want: map[string]string{"db.operation.name": "BEGIN", "db.query.summary": "BEGIN"}},
		{name: "statement stating no summary", sql: "WITH matched AS (SELECT 1) SELECT * FROM matched",
			wantName: "postgresql", want: map[string]string{}},
		{name: "preparation", sql: lockSQL, prepare: true, wantName: "prepare lock targets",
			want: map[string]string{"db.query.summary": "lock targets"}, noMetric: true},
		{name: "preparation stating no summary", sql: "SELECT 1", prepare: true, wantName: "prepare postgresql",
			want: map[string]string{}, noMetric: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepare {
				tracer.TracePrepareEnd(tracer.TracePrepareStart(ctx, nil, pgx.TracePrepareStartData{SQL: tt.sql}),
					nil, pgx.TracePrepareEndData{Err: tt.err})
			} else {
				tracer.TraceQueryEnd(tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: tt.sql}),
					nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag(tt.tag), Err: tt.err})
			}

			spans := recorder.Ended()
			span := spans[len(spans)-1]
			if got := span.Name(); got != tt.wantName {
				t.Errorf("span name = %q, want %q", got, tt.wantName)
			}
			if got := span.Status().Code; got != tt.wantStatus {
				t.Errorf("span status = %v, want %v", got, tt.wantStatus)
			}
			got := map[string]map[string]string{"span": attributes(span.Attributes())}
			points := statementPoints(t, reader)
			switch {
			case tt.noMetric && len(points) != 0:
				t.Errorf("db.client.operation.duration points = %d, want none", len(points))
			case !tt.noMetric && len(points) != 1:
				t.Fatalf("db.client.operation.duration points = %d, want the statement's", len(points))
			case !tt.noMetric:
				got["metric"] = attributes(points[0].Attributes.ToSlice())
			}
			want := map[string]map[string]string{"span": {"test.span_only": "yes"}, "metric": {}}
			for _, expected := range []map[string]string{{"db.system.name": "postgresql", "db.namespace": "ledger",
				"server.address": "db", "server.port": "5433"}, tt.want} {
				maps.Copy(want["span"], expected)
				maps.Copy(want["metric"], expected)
			}
			maps.Copy(want["span"], tt.wantRows)
			if text := got["span"]["db.query.text"]; text != tt.sql {
				t.Errorf("span db.query.text = %q, want %q", text, tt.sql)
			}
			// The metric must not carry the SQL, so its set stays whole.
			delete(got["span"], "db.query.text")
			for name, got := range got {
				if !maps.Equal(got, want[name]) {
					t.Errorf("%s attributes = %v, want %v", name, got, want[name])
				}
			}
		})
	}
}

// statementPoints returns the db.client.operation.duration points collected since the previous
// collection.
func statementPoints(t *testing.T, reader sdkmetric.Reader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	var points []metricdata.HistogramDataPoint[float64]
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if histogram, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == "db.client.operation.duration" {
				points = append(points, histogram.DataPoints...)
			}
		}
	}
	return points
}

func attributes(kvs []attribute.KeyValue) map[string]string {
	attrs := map[string]string{}
	for _, kv := range kvs {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	return attrs
}
