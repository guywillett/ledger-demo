package db

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// queryTracer gives each statement run inside a span a child span of its own: the contract's
// database spans (contracts/telemetry.md), as Node's pg instrumentation makes them. pgx calls a
// tracer set on the connection's config (pgx.QueryTracer) at the start and end of every statement;
// our own rather than a third-party one, as a pgx tracer is two methods.
//
// A statement outside any span gets no span: that's the relay's and the consumers' polling, which
// would otherwise be a trace a second per process (slice 5 decision 85; Node: requireParentSpan).
// The statement's text, never its arguments (the contract's privacy rule).
//
// The tracer is the global one (otel.Tracer), looked up per statement: a no-op until
// telemetry.Setup registers a provider, so nothing is recorded with telemetry off.
type queryTracer struct{}

var _ pgx.QueryTracer = queryTracer{}

// The instrumentation scope's name, as Go's conventions have it: the package's import path.
const tracerName = "github.com/guywillett/ledger-demo/go/internal/db"

// querySpanKey marks the span TraceQueryStart started, so TraceQueryEnd ends that one and never a
// caller's (the context's span when TraceQueryStart started none).
type querySpanKey struct{}

// TraceQueryStart starts the statement's span, if it runs inside one.
func (queryTracer) TraceQueryStart(
	ctx context.Context,
	conn *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	operation := StatementOperation(data.SQL)
	config := conn.Config()
	ctx, span := otel.Tracer(tracerName).Start(ctx, operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.namespace", config.Database),
			attribute.String("db.operation.name", operation),
			attribute.String("db.query.text", data.SQL),
			attribute.String("server.address", config.Host),
			attribute.Int("server.port", int(config.Port)),
		))
	return context.WithValue(ctx, querySpanKey{}, span)
}

// DatabaseError is a failed statement's status description: fixed, never the error's message,
// which can quote the values sent (22P02: invalid input syntax for type uuid: "…"), and which a
// recorded error (span.RecordError) would put in an exception event. The SQLSTATE says what
// happened. Node: DATABASE_ERROR in node/src/shared/telemetry.ts.
const DatabaseError = "database error"

// TraceQueryEnd ends the statement's span: if it failed, the SQLSTATE as error.type and an error
// status with a fixed description.
func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(querySpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil {
		// errors.AsType finds a *pgconn.PgError anywhere in the chain (Node: err.code on pg's error).
		if pgErr, ok := errors.AsType[*pgconn.PgError](data.Err); ok {
			span.SetAttributes(attribute.String("error.type", pgErr.Code))
		}
		span.SetStatus(codes.Error, DatabaseError)
	}
	span.End()
}

// The first keyword of a statement.
var operationPattern = regexp.MustCompile(`^\s*([A-Za-z]+)`)

// StatementOperation is a statement's operation, its first keyword upper-cased (SELECT, INSERT,
// UPDATE, WITH, BEGIN, …), "QUERY" if it starts with anything else: the database span's name.
// Every statement is a constant in our code, so the names are a bounded set. Node:
// statementOperation in node/src/shared/telemetry.ts.
func StatementOperation(sql string) string {
	m := operationPattern.FindStringSubmatch(sql)
	if m == nil {
		return "QUERY"
	}
	return strings.ToUpper(m[1])
}
