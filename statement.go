package pgxtel

import (
	"slices"
	"strings"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Statement is the span name and attributes a statement, and its preparation, is recorded under.
type Statement struct {
	// SpanName names the statement's span; its preparation's span takes it prefixed by "prepare".
	SpanName string
	// SpanAttrs go on the spans alone, so they may be of high cardinality.
	SpanAttrs []attribute.KeyValue
	// Attrs go on the spans and the duration metric, so they must be of low cardinality.
	Attrs []attribute.KeyValue
}

// Describer adjusts the Statement the defaults describe the statement sql by. The database's
// identity, the query text, row counts, and failure attributes are recorded besides it.
type Describer func(sql string, s *Statement)

// transactionControl are the statements pgx issues for database/sql transactions; each is a single
// operation, so its first word may name it, unlike a query, which may combine several.
var transactionControl = []string{"BEGIN", "COMMIT", "ROLLBACK"}

// defaultStatement names transaction control by its operation, which it also records as both
// db.operation.name and db.query.summary, and any other statement by the database system alone.
func defaultStatement(sql string) Statement {
	operation, _, _ := strings.Cut(strings.TrimLeftFunc(sql, unicode.IsSpace), " ")
	if operation = strings.ToUpper(operation); slices.Contains(transactionControl, operation) {
		return Statement{SpanName: operation,
			Attrs: []attribute.KeyValue{semconv.DBOperationName(operation), semconv.DBQuerySummary(operation)}}
	}
	return Statement{SpanName: semconv.DBSystemNamePostgreSQL.Value.AsString()}
}

// CommentSummary is the Describer naming a statement that opens with a non-empty /* block comment */
// by the comment's text, which it also records as db.query.summary: the application states a
// low-cardinality summary in each query instead of a parser deriving one from the SQL.
func CommentSummary(sql string, s *Statement) {
	comment, ok := strings.CutPrefix(strings.TrimLeftFunc(sql, unicode.IsSpace), "/*")
	if !ok {
		return
	}
	text, _, closed := strings.Cut(comment, "*/")
	if summary := strings.TrimSpace(text); closed && summary != "" {
		s.SpanName = summary
		s.Attrs = append(s.Attrs, semconv.DBQuerySummary(summary))
	}
}
