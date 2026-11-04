// Package pgxtel traces the statements, preparations, and connection setups of a pgx connection,
// measures each statement's duration, and reports a database/sql connection pool, all under the
// OpenTelemetry database semantic conventions.
//
// File map:
//   - tracer.go: the pgx tracer behind the spans and the statement duration metric.
//   - statement.go: a statement's span name and attributes — the defaults, the Describer hook
//     adjusting them, and the CommentSummary describer.
//   - pool.go: the connection pool metrics.
package pgxtel
