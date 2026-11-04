package pgxtel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/dbconv"
	"go.opentelemetry.io/otel/trace"
)

// scope names the tracer and meter of the package's telemetry.
const scope = "github.com/CthulhuDen/pgxtel"

// statementDurationBuckets are the bucket boundaries, in seconds, the semantic conventions advise for
// db.client.operation.duration.
var statementDurationBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10}

// rowsAffectedKey carries the row count of a statement's command tag, in pgx's namespace: semantic
// conventions name only the rows a select returns.
const rowsAffectedKey = attribute.Key("pgx.rows_affected")

// Tracer is the pgx.ConnConfig.Tracer recording a client span per statement, preparation, and
// connection setup, and each statement's db.client.operation.duration, through the global tracer and
// meter providers. A failed span or measurement carries a server error's SQLSTATE as error.type and
// db.response.status_code, or _OTHER as error.type for any other failure. A successful statement span
// carries a select, insert, update, or delete tag's row count as pgx.rows_affected, and a select's
// also as db.response.returned_rows.
type Tracer struct {
	tracer   trace.Tracer
	meter    metric.Meter
	duration dbconv.ClientOperationDuration
	describe Describer
	// database identifies the database server on every span and measurement.
	database []attribute.KeyValue
}

// Option configures a Tracer.
type Option func(*Tracer)

// WithDescriber has describe adjust each statement's Statement after the defaults describe it.
func WithDescriber(describe Describer) Option {
	return func(t *Tracer) { t.describe = describe }
}

// New returns the Tracer of connections to the database config names.
func New(config *pgx.ConnConfig, opts ...Option) (*Tracer, error) {
	meter := otel.Meter(scope)
	duration, err := dbconv.NewClientOperationDuration(meter,
		metric.WithExplicitBucketBoundaries(statementDurationBuckets...))
	if err != nil {
		return nil, fmt.Errorf("create SQL duration metric: %w", err)
	}
	t := &Tracer{tracer: otel.Tracer(scope), meter: meter, duration: duration, database: []attribute.KeyValue{
		semconv.DBSystemNamePostgreSQL,
		semconv.DBNamespace(config.Database),
		semconv.ServerAddress(config.Host),
		semconv.ServerPort(int(config.Port)),
	}}
	for _, opt := range opts {
		opt(t)
	}
	return t, nil
}

// pgx discovers the preparation and connection hooks by asserting ConnConfig.Tracer, which is typed
// QueryTracer alone, so only this pins them.
var _ interface {
	pgx.QueryTracer
	pgx.PrepareTracer
	pgx.ConnectTracer
} = (*Tracer)(nil)

// statementKey carries a statement's start and measurement attributes from its start hook to its end
// hook.
type statementKey struct{}

type statement struct {
	start time.Time
	attrs []attribute.KeyValue
}

func (t *Tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s, attrs := t.statement(data.SQL)
	ctx = t.start(ctx, s.SpanName, data.SQL, attrs, s.SpanAttrs)
	return context.WithValue(ctx, statementKey{}, statement{start: time.Now(), attrs: attrs})
}

func (t *Tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	s := ctx.Value(statementKey{}).(statement)
	attrs := append(s.attrs, failureAttributes(data.Err)...)
	t.duration.Inst().Record(ctx, time.Since(s.start).Seconds(), metric.WithAttributes(attrs...))
	if tag := data.CommandTag; data.Err == nil && (tag.Select() || tag.Insert() || tag.Update() || tag.Delete()) {
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(rowsAffectedKey.Int64(tag.RowsAffected()))
		// Only a select's count is certainly of returned rows; a write's RETURNING leaves it ambiguous.
		if tag.Select() {
			span.SetAttributes(semconv.DBResponseReturnedRows(int(tag.RowsAffected())))
		}
	}
	endSpan(ctx, data.Err)
}

func (t *Tracer) TracePrepareStart(ctx context.Context, _ *pgx.Conn, data pgx.TracePrepareStartData) context.Context {
	s, attrs := t.statement(data.SQL)
	return t.start(ctx, "prepare "+s.SpanName, data.SQL, attrs, s.SpanAttrs)
}

func (t *Tracer) TracePrepareEnd(ctx context.Context, _ *pgx.Conn, data pgx.TracePrepareEndData) {
	endSpan(ctx, data.Err)
}

func (t *Tracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	ctx, _ = t.tracer.Start(ctx, "connect", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(t.database...))
	return ctx
}

func (t *Tracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	endSpan(ctx, data.Err)
}

// statement returns the Statement of sql and the attributes its spans and measurement share: the
// database's and the Statement's own. The attributes are a fresh slice, which no other statement
// shares.
func (t *Tracer) statement(sql string) (Statement, []attribute.KeyValue) {
	s := defaultStatement(sql)
	if t.describe != nil {
		t.describe(sql, &s)
	}
	return s, append(slices.Clip(t.database), s.Attrs...)
}

func (t *Tracer) start(ctx context.Context, name, sql string, attrs, spanAttrs []attribute.KeyValue) context.Context {
	ctx, _ = t.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...),
		trace.WithAttributes(spanAttrs...), trace.WithAttributes(semconv.DBQueryText(sql)))
	return ctx
}

// endSpan ends the span in ctx, failed when err is set.
func endSpan(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if err != nil {
		span.SetAttributes(failureAttributes(err)...)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// failureAttributes returns the attributes classifying err: the SQLSTATE a server error carries,
// _OTHER for any other failure, none for success.
func failureAttributes(err error) []attribute.KeyValue {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return []attribute.KeyValue{semconv.ErrorTypeKey.String(pgErr.Code), semconv.DBResponseStatusCode(pgErr.Code)}
	}
	return []attribute.KeyValue{semconv.ErrorTypeOther}
}
