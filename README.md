# pgxtel

OpenTelemetry tracing and metrics for [pgx v5](https://github.com/jackc/pgx) under the database semantic
conventions: a client span per statement, preparation, and connection setup, `db.client.operation.duration`
per statement, and the `db.client.connection.*` metrics of a `database/sql` or `pgxpool` pool.

```go
config, err := pgx.ParseConfig(databaseURL)
tracer, err := pgxtel.New(config, pgxtel.WithDescriber(pgxtel.CommentSummary))
config.Tracer = tracer
db := stdlib.OpenDB(*config)
registration, err := tracer.ObservePool(db, "main")
```

With `pgxpool`, the tracer goes on the pool's connection config and `ObservePgxPool` reports the pool:

```go
config, err := pgxpool.ParseConfig(databaseURL)
tracer, err := pgxtel.New(config.ConnConfig, pgxtel.WithDescriber(pgxtel.CommentSummary))
config.ConnConfig.Tracer = tracer
pool, err := pgxpool.NewWithConfig(ctx, config)
registration, err := tracer.ObservePgxPool(pool, "main")
```

A `Describer` adjusts each statement's span name and attributes: `SpanAttrs` go on the spans alone, `Attrs` on
the spans and the duration metric. Without one, transaction control is named by its operation and any other
statement `postgresql`. `CommentSummary` names a statement opening with `/* summary */` by that text and records
it as `db.query.summary`.
