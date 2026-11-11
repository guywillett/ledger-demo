package telemetry_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/telemetry"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
)

// Setup's logger provider: the log lines' copy to the backend (slice 5 decision 93). Node: the
// startTelemetry cases in test/telemetry/logs*.telemetry.test.ts. What a logger sends it (the
// fan-out, the mapping) is internal/logging's otel_test.go.

// Batched, as spans are (decision 79): by stop at the latest, which flushes the logs too. Through
// ContractLogs: a line from a logger bound to a span has the span's IDs as the record's own, and
// its level's name as the severity text.
func TestALogRecordReachesTheLogExporterByStopWithTheResourceAndTheContractsShape(t *testing.T) {
	records := &telemetrytest.LogExporter{}
	stop := setup(t, telemetry.Config{
		Service:      "relay",
		Logs:         "otlp",
		SpanExporter: &recordingExporter{},
		LogExporter:  records,
	})
	// Made after Setup, as in a process the global provider is the one Setup registered.
	_, logger := newLogger(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "work")
	logging.WithTrace(ctx, logger).Info("published", "event_id", "evt_1")
	span.End()
	stop()

	exported := records.Exported()
	if len(exported) != 1 {
		t.Fatalf("%d records exported, want 1", len(exported))
	}
	r := exported[0]
	if r.Body().AsString() != "published" || r.Severity() != log.SeverityInfo || r.SeverityText() != "info" {
		t.Errorf("record %q %v %q, want published INFO info", r.Body().AsString(), r.Severity(), r.SeverityText())
	}
	if r.TraceID() != span.SpanContext().TraceID() || r.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("record trace %v span %v, want the span's", r.TraceID(), r.SpanID())
	}
	if attrs := telemetrytest.LogAttributes(r); len(attrs) != 1 || attrs["event_id"].AsString() != "evt_1" {
		t.Errorf("attributes %v, want event_id only", attrs)
	}
	got := map[string]string{}
	for _, kv := range r.Resource().Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got["service.name"] != "relay" || got["implementation"] != "go" || got["service.instance.id"] == "" {
		t.Errorf("resource %v, want Setup's", got)
	}
}

// OTEL_LOGS_EXPORTER=none: stdout only. Traces are still exported; no logger provider is
// registered, so a logger has no backend side.
func TestLogsNoneExportsNoLogRecords(t *testing.T) {
	records := &telemetrytest.LogExporter{}
	spans := &recordingExporter{}
	stop := setup(t, telemetry.Config{Logs: "none", SpanExporter: spans, LogExporter: records})
	l, logger := newLogger(t)
	logger.Info("stdout only")
	_, span := otel.Tracer("test").Start(context.Background(), "a span")
	span.End()
	stop()

	if n := len(records.Exported()); n != 0 {
		t.Errorf("%d records exported, want none", n)
	}
	if n := len(spans.exported()); n != 1 {
		t.Errorf("%d spans exported, want 1", n)
	}
	if lines := l.lines(t); len(lines) != 1 || lines[0]["msg"] != "stdout only" {
		t.Errorf("stdout %v, want the line", lines)
	}
	if _, ok := otel.GetLoggerProvider().(*sdklog.LoggerProvider); ok {
		t.Error("a logger provider is registered")
	}
}

// Logs is OTEL_LOGS_EXPORTER as config gives it, "otlp" or "none": anything else is a
// programmer's error (a process whose main doesn't pass it, ""), refused at startup rather than
// taken for either. Called directly, not through setup, which fills Logs in.
func TestLogsOtherThanOTLPOrNoneIsRefused(t *testing.T) {
	for _, logs := range []string{"", "console"} {
		t.Run(logs, func(t *testing.T) {
			restoreGlobals(t)
			_, logger := newLogger(t)
			_, err := telemetry.Setup(context.Background(), telemetry.Config{
				Service:      "api",
				Logger:       logger,
				Logs:         logs,
				SpanExporter: &recordingExporter{},
				MetricReader: sdkmetric.NewManualReader(),
				LogExporter:  &telemetrytest.LogExporter{},
			})
			if err == nil {
				t.Errorf("Logs %q accepted, want an error", logs)
			}
		})
	}
}

// A test's exporters come as a set, all three or none: with only some, the others would be real
// OTLP exporters, sending to localhost:4318 from a test (the span/metric pair's rule, extended).
func TestAnIncompleteSetOfTestExportersIsRefused(t *testing.T) {
	sets := map[string]telemetry.Config{
		"no log exporter":     {SpanExporter: &recordingExporter{}, MetricReader: sdkmetric.NewManualReader()},
		"no metric reader":    {SpanExporter: &recordingExporter{}, LogExporter: &telemetrytest.LogExporter{}},
		"a log exporter only": {LogExporter: &telemetrytest.LogExporter{}},
	}
	for name, cfg := range sets {
		t.Logf("%v", name)
		t.Run(name, func(t *testing.T) {
			restoreGlobals(t)
			_, cfg.Logger = newLogger(t)
			cfg.Service, cfg.Logs = "api", "otlp"
			if _, err := telemetry.Setup(context.Background(), cfg); err == nil {
				t.Error("accepted, want an error")
			}
		})
	}
}

// Stop's grace covers the logs too: an exporter that never answers costs at most the grace, with
// the one warning (as TestStopGivesUpAfterItsGraceAndSaysSo for spans).
func TestStopGivesUpOnLogsAfterItsGraceAndSaysSo(t *testing.T) {
	l, logger := newLogger(t)
	stop := setup(t, telemetry.Config{
		SpanExporter:  &recordingExporter{},
		LogExporter:   blockingLogExporter{},
		Logger:        logger,
		ShutdownGrace: 200 * time.Millisecond,
	})
	_, emitting := newLogger(t)
	emitting.Info("never exported")
	start := time.Now()
	stop()
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stop took %v", took)
	}
	lines := l.lines(t)
	if len(lines) != 1 || lines[0]["msg"] != "telemetry not flushed; dropped" {
		t.Errorf("lines %v, want the one warning", lines)
	}
}

// blockingLogExporter never finishes an export until its context ends: a backend that doesn't
// answer.
type blockingLogExporter struct{}

func (blockingLogExporter) Export(ctx context.Context, _ []sdklog.Record) error {
	<-ctx.Done()
	return ctx.Err()
}

func (blockingLogExporter) Shutdown(context.Context) error { return nil }

func (blockingLogExporter) ForceFlush(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
