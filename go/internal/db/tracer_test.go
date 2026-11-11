package db_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The database tracer (queryTracer, set by Connect), against the test database: a span per statement
// inside a span and none outside one, the statement's text and never its arguments, a failure as an
// error status. Node: its pg instrumentation, configured in node/src/shared/telemetry.ts.

func TestAStatementOutsideAnySpanGetsNoSpan(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	if err := db.Ping(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if spans := rec.Spans(); len(spans) != 0 {
		t.Errorf("%d spans, want none", len(spans))
	}
}

func TestAStatementInsideASpanGetsAChildSpanWithItsTextAndNoArguments(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	var out string
	if err := pool.QueryRow(ctx, "SELECT $1::text", "an-argument").Scan(&out); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := rec.Spans()
	if len(spans) != 2 || spans[0].Name() != "SELECT" {
		t.Fatalf("spans %v", spans)
	}
	query := spans[0]
	attrs := telemetrytest.Attributes(query)
	if query.Parent().SpanID() != parent.SpanContext().SpanID() ||
		attrs["db.system.name"].AsString() != "postgresql" ||
		attrs["db.namespace"].AsString() != "ledger_go_test" ||
		attrs["db.operation.name"].AsString() != "SELECT" ||
		attrs["db.query.text"].AsString() != "SELECT $1::text" {
		t.Errorf("span %s %v", query.Name(), attrs)
	}
	for _, kv := range query.Attributes() {
		if strings.Contains(kv.Value.String(), "an-argument") {
			t.Errorf("the argument in %s", kv.Key)
		}
	}
}

// 22P02 quotes the value it refused in its message: the span keeps the SQLSTATE and an error status
// with a fixed description, and no exception event, so the value is nowhere in it.
func TestAFailedStatementKeepsItsSQLSTATEAndAnErrorStatusNeverItsMessageOrTheValue(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	value := "not-a-uuid-" + ids.New[ids.Event]().TypeID()
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	_, err := pool.Exec(ctx, "SELECT $1::uuid", value)
	parent.End()
	if err == nil {
		t.Fatal("no error")
	}
	query := rec.Spans()[0]
	if got := telemetrytest.Attributes(query)["error.type"].AsString(); got != "22P02" {
		t.Errorf("error.type %q, want 22P02", got)
	}
	if query.Status().Code != codes.Error || query.Status().Description != db.DatabaseError {
		t.Errorf("status %+v", query.Status())
	}
	if len(query.Events()) != 0 {
		t.Errorf("events %v, want none", query.Events())
	}
	raw, _ := json.Marshal(struct {
		Attributes any
		Events     any
		Status     any
	}{query.Attributes(), query.Events(), query.Status()})
	if strings.Contains(string(raw), value) {
		t.Errorf("the value in %s", raw)
	}
}

func TestStatementOperationNamesAStatementByItsFirstKeyword(t *testing.T) {
	tests := []struct{ sql, want string }{
		{"SELECT id FROM accounts WHERE id = $1", "SELECT"},
		{"\n  insert into deposits (id) values ($1)", "INSERT"},
		{"WITH claimed AS (SELECT 1) UPDATE outbox SET x = 1", "WITH"},
		{"BEGIN ISOLATION LEVEL REPEATABLE READ", "BEGIN"},
		{"-- a comment first", "QUERY"},
	}
	for _, tt := range tests {
		if got := db.StatementOperation(tt.sql); got != tt.want {
			t.Errorf("%q: %s, want %s", tt.sql, got, tt.want)
		}
	}
}
