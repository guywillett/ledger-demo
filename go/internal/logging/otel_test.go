package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
)

// The log lines' copy to the telemetry backend (slice 5 decision 93; contracts/telemetry.md,
// Logs): every line New's logger writes to stdout also reaches the global logger provider, as an
// OpenTelemetry log record with the contract's mapping, through telemetrytest's recording provider
// (telemetry.ContractLogs, then an in-memory exporter, as Setup's). Node:
// test/telemetry/logs.telemetry.test.ts.

// logger is New's logger writing to the returned buffer, made after rec so it exports there.
func logger(t *testing.T, level string) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l, err := logging.New(&buf, "deposit-worker", level)
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

// telemetryOff sets the global logger provider to a no-op, as a process without telemetry has: a
// logger made then has no backend side. Set explicitly, because before any provider is set the
// global hands out its delegate, which follows the first provider set later (a Record's).
func telemetryOff() {
	otel.SetLoggerProvider(lognoop.NewLoggerProvider())
}

// only is the one record exported, failing the test if there isn't exactly one.
func only(t *testing.T, rec *telemetrytest.Recorder) sdklog.Record {
	t.Helper()
	records := rec.Logs()
	if len(records) != 1 {
		t.Fatalf("%d records exported, want 1", len(records))
	}
	return records[0]
}

func TestALineReachesStdoutAndTheBackendWithTheSameMessageLevelAndFields(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, buf := logger(t, "info")
	l.With("deposit_id", "dep_1").
		Warn("retrying", "attempt", 2, "amount_minor", money.Amount(1250), "level", "high")

	line := lines(t, buf)[0]
	for key, want := range map[string]any{
		"msg": "retrying", "level": "warn", "deposit_id": "dep_1", "attempt": float64(2),
		"amount_minor": "1250", "attr.level": "high", "service": "deposit-worker",
	} {
		if line[key] != want {
			t.Errorf("stdout %s = %v, want %v", key, line[key], want)
		}
	}

	record := only(t, rec)
	if record.Body().AsString() != "retrying" {
		t.Errorf("body %q", record.Body().AsString())
	}
	if record.Severity() != log.SeverityWarn || record.SeverityText() != "warn" {
		t.Errorf("severity %v %q, want WARN warn", record.Severity(), record.SeverityText())
	}
	// The line's own fields, by their stdout names; not service and implementation (the
	// resource's), nor time, level and msg (the record's own fields).
	want := map[attribute.Key]attribute.Value{
		"deposit_id":   attribute.StringValue("dep_1"),
		"attempt":      attribute.Int64Value(2),
		"amount_minor": attribute.StringValue("1250"),
		"attr.level":   attribute.StringValue("high"),
	}
	if got := telemetrytest.LogAttributes(record); !reflect.DeepEqual(got, want) {
		t.Errorf("attributes %v, want %v", got, want)
	}
}

func TestAGroupReachesTheBackendAsAMap(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, buf := logger(t, "info")
	l.WithGroup("provider").Info("answered", "status", 503)

	if got := lines(t, buf)[0]["provider"]; !reflect.DeepEqual(got, map[string]any{"status": float64(503)}) {
		t.Errorf("stdout provider %v", got)
	}
	want := map[attribute.Key]attribute.Value{
		"provider": attribute.MapValue(attribute.Int64("status", 503)),
	}
	if got := telemetrytest.LogAttributes(only(t, rec)); !reflect.DeepEqual(got, want) {
		t.Errorf("attributes %v, want %v", got, want)
	}
}

// A copy of stdout, so not a line stdout doesn't have: the backend side keeps the logger's level,
// on a logger bound with With or WithGroup too.
func TestALineBelowTheLevelIsNeitherWrittenNorExportedFromABoundOrGroupedLoggerToo(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, buf := logger(t, "info")
	l.Debug("plain")
	l.With("deposit_id", "dep_1").Debug("bound")
	l.WithGroup("provider").Debug("grouped")
	l.With("deposit_id", "dep_1").WithGroup("provider").With("status", 503).Debug("both")
	if buf.Len() != 0 {
		t.Errorf("stdout %q, want nothing", buf.String())
	}
	if records := rec.Logs(); len(records) != 0 {
		t.Errorf("%d records exported, want none", len(records))
	}
}

func TestEnabledIsTrueIfEitherSideIs(t *testing.T) {
	ctx := context.Background()
	// Telemetry off: the backend side is disabled, stdout's isn't.
	telemetryOff()
	off, buf := logger(t, "info")
	if !off.Enabled(ctx, slog.LevelInfo) {
		t.Error("disabled at info with telemetry off")
	}
	off.Info("still on stdout")
	if len(lines(t, buf)) != 1 {
		t.Errorf("stdout %q, want the line", buf.String())
	}

	rec := telemetrytest.Record(t)
	on, _ := logger(t, "info")
	if !on.Enabled(ctx, slog.LevelInfo) || on.Enabled(ctx, slog.LevelDebug) {
		t.Error("want enabled at info, not at debug")
	}
	on.Info("both")
	if len(rec.Logs()) != 1 {
		t.Errorf("%d records exported, want 1", len(rec.Logs()))
	}
}

// The two levels slog lacks (LevelTrace, LevelFatal) are OpenTelemetry's TRACE and FATAL, named as
// on stdout.
func TestTraceAndFatalAreTheBackendsTraceAndFatal(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, _ := logger(t, "trace")
	l.Log(context.Background(), logging.LevelTrace, "tracing")
	logging.Fatal(context.Background(), l, "startup failed")
	var got []string
	for _, r := range rec.Logs() {
		got = append(got, r.Severity().String()+" "+r.SeverityText())
	}
	if want := []string{"TRACE trace", "FATAL fatal"}; !reflect.DeepEqual(got, want) {
		t.Errorf("severities %v, want %v", got, want)
	}
}

// Decision 86's IDs, bound on the logger (logging.WithTrace) as a request's and a delivery's are:
// the record's own trace and span IDs, as Node's, not attributes (telemetry.ContractLogs).
func TestALineFromALoggerBoundToASpanHasItsTraceAndSpanIDs(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, buf := logger(t, "info")
	ctx, span := otel.Tracer("test").Start(context.Background(), "work")
	logging.WithTrace(ctx, l).Info("in a span") // no context, as most of our code logs
	span.End()

	if line := lines(t, buf)[0]; line["trace_id"] != span.SpanContext().TraceID().String() {
		t.Errorf("stdout trace_id %v", line["trace_id"])
	}
	record := only(t, rec)
	if record.TraceID() != span.SpanContext().TraceID() || record.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("record trace %v span %v, want the span's", record.TraceID(), record.SpanID())
	}
	if attrs := telemetrytest.LogAttributes(record); len(attrs) != 0 {
		t.Errorf("attributes %v, want none (the IDs are the record's own)", attrs)
	}
}

// A line logged with a context (InfoContext) in a span started after the logger was bound: that
// span is the nearer one, so the record has its IDs.
func TestAnInfoContextLinesSpanIsTheNearer(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, _ := logger(t, "info")
	ctx, parent := otel.Tracer("test").Start(context.Background(), "delivery")
	bound := logging.WithTrace(ctx, l)
	childCtx, child := otel.Tracer("test").Start(ctx, "step")
	bound.InfoContext(childCtx, "in the child")
	child.End()
	parent.End()

	record := only(t, rec)
	if record.SpanID() != child.SpanContext().SpanID() {
		t.Errorf("span %v, want the child's %v", record.SpanID(), child.SpanContext().SpanID())
	}
	if attrs := telemetrytest.LogAttributes(record); len(attrs) != 0 {
		t.Errorf("attributes %v, want none", attrs)
	}
}

type overdraftError struct{}

func (overdraftError) Error() string { return "insufficient funds" }

// An error is OpenTelemetry's exception (otelslog and the SDK do this), not an "err" attribute.
func TestAnErrorIsTheBackendsException(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, _ := logger(t, "info")
	l.Error("hold failed", "err", overdraftError{})
	attrs := telemetrytest.LogAttributes(only(t, rec))
	if attrs["exception.message"].AsString() != "insufficient funds" ||
		!strings.HasSuffix(attrs["exception.type"].AsString(), "/internal/logging_test.overdraftError") {
		t.Errorf("attributes %v, want the error as exception.type and exception.message", attrs)
	}
	if _, ok := attrs["err"]; ok {
		t.Errorf("attributes %v, want no err", attrs)
	}
}

// The same lines, with telemetry off and with the copy on, are the same bytes (but the time, which
// is the clock's).
func TestStdoutsLinesAreUnchangedByTheCopy(t *testing.T) {
	write := func(l *slog.Logger) {
		ctx, span := otel.Tracer("test").Start(context.Background(), "work")
		defer span.End()
		bound := logging.WithTrace(ctx, l).With("deposit_id", "dep_1")
		bound.Info("booked", "amount_minor", money.Amount(1250), "level", "high")
		bound.WithGroup("provider").Warn("answered", "status", 503)
		bound.Error("hold failed", "err", overdraftError{})
		logging.Fatal(ctx, bound, "stopping")
	}
	// The trace and span IDs differ between the two runs, as the time does.
	varying := regexp.MustCompile(`"(time|trace_id|span_id)":"[^"]*"`)
	normalised := func(buf *bytes.Buffer) string { return varying.ReplaceAllString(buf.String(), `"$1":"…"`) }

	telemetryOff()
	off, offBuf := logger(t, "info")
	rec := telemetrytest.Record(t)
	on, onBuf := logger(t, "info")
	write(off)
	write(on)
	if len(rec.Logs()) != 4 {
		t.Fatalf("%d records exported, want the copying logger's 4", len(rec.Logs()))
	}

	if normalised(onBuf) != normalised(offBuf) {
		t.Errorf("with the copy:\n%s\nwithout:\n%s", onBuf, offBuf)
	}
	if !strings.Contains(onBuf.String(), `"trace_id":"`) {
		t.Error("no trace_id on stdout")
	}
}

func TestSilentExportsNothing(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, _ := logger(t, "silent")
	l.Error("never")
	if len(rec.Logs()) != 0 {
		t.Errorf("%d records exported, want none", len(rec.Logs()))
	}
}

// Named types with a basic underlying type, as withdrawals.Status (a string) and ingestion.Kind (an
// int) are: otelslog would write them "unhandled: (logging_test.status) executed".
type (
	status string
	count  int
	small  uint8
	ratio  float64
	flag   bool
)

// A named type's value reaches the backend as stdout writes it, the plain value: from a log call, a
// With, after WithGroup and inside an inline group (plainValues).
func TestANamedTypesValueReachesTheBackendAsStdoutWritesIt(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, buf := logger(t, "info")
	l.With("bound", status("held")).WithGroup("g").Info("moved",
		"status", status("executed"), "count", count(3), "small", small(7), "ratio", ratio(0.5),
		"flag", flag(true), slog.Group("provider", "status", status("failed")))

	line := lines(t, buf)[0]
	if line["bound"] != "held" ||
		!reflect.DeepEqual(line["g"], map[string]any{
			"status": "executed", "count": float64(3), "small": float64(7), "ratio": 0.5, "flag": true,
			"provider": map[string]any{"status": "failed"},
		}) {
		t.Fatalf("stdout %v", line)
	}
	want := map[attribute.Key]attribute.Value{
		"bound": attribute.StringValue("held"),
		"g": attribute.MapValue(
			attribute.String("status", "executed"),
			attribute.Int64("count", 3),
			attribute.Int64("small", 7),
			attribute.Float64("ratio", 0.5),
			attribute.Bool("flag", true),
			attribute.Map("provider", attribute.String("status", "failed")),
		),
	}
	if got := telemetrytest.LogAttributes(only(t, rec)); !reflect.DeepEqual(got, want) {
		t.Errorf("attributes %v\nwant       %v", got, want)
	}
}

type failure string

func (f failure) Error() string { return string(f) }

// An error is still the record's exception, though its type is a named string.
func TestANamedStringErrorIsStillTheBackendsException(t *testing.T) {
	rec := telemetrytest.Record(t)
	l, _ := logger(t, "info")
	l.Error("hold failed", "err", failure("insufficient funds"))
	attrs := telemetrytest.LogAttributes(only(t, rec))
	if attrs["exception.message"].AsString() != "insufficient funds" {
		t.Errorf("attributes %v, want the error as the exception", attrs)
	}
	if _, ok := attrs["err"]; ok {
		t.Errorf("attributes %v, want no err", attrs)
	}
}
