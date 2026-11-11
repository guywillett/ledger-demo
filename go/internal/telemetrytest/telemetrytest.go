// Package telemetrytest records the spans, metrics and log records that the code under test makes,
// in memory: the Go counterpart of Node's InMemorySpanExporter, InMemoryMetricExporter and
// InMemoryLogRecordExporter in test/telemetry/. Imported only by tests, like testdb and relaytest.
package telemetrytest

import (
	"context"
	"slices"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/guywillett/ledger-demo/go/internal/telemetry"
)

// Recorder holds what was recorded since Record.
type Recorder struct {
	spans  *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
	logs   *LogExporter
}

// Record sets the global tracer, meter and logger providers to ones recording in memory, and the
// W3C propagators, as telemetry.Setup sets the real ones in a process, and sets no-op ones back
// when the test ends.
// Global, so the code records through the providers it finds at run time (otelhttp, the database
// tracer); safe because no test in this module calls t.Parallel, so a package's tests run one at a
// time. Build whatever captures a provider (an otelhttp handler) after calling it.
//
// Spans are exported as they end (WithSyncer), through the same filter as Setup's exporter
// (telemetry.WithoutPersonalData), and log records as they are emitted (a simple processor), after
// the same reshaping as Setup's (telemetry.ContractLogs), so a test sees what a process exports. A
// logger (logging.New) takes the global logger provider when it is made: make it after Record.
func Record(t testing.TB) *Recorder {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(telemetry.WithoutPersonalData(spans)))
	reader := sdkmetric.NewManualReader()
	// The contract's views (an allow-list), as Setup's: a metric they drop isn't exported.
	meters := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(telemetry.ContractMetrics),
	)
	logs := &LogExporter{}
	loggers := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(telemetry.ContractLogs{}),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)),
	)
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(meters)
	otel.SetLoggerProvider(loggers)
	// The W3C propagators, as Setup registers them: the outbox and the relay write trace context
	// through the global propagator, which is a no-op until one is set.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Cleanup(func() {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		otel.SetLoggerProvider(lognoop.NewLoggerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
		_ = traces.Shutdown(context.Background())
		_ = meters.Shutdown(context.Background())
		_ = loggers.Shutdown(context.Background())
	})
	return &Recorder{spans: spans, reader: reader, logs: logs}
}

// Logs are the log records emitted so far, in order, as exported.
func (r *Recorder) Logs() []sdklog.Record {
	return r.logs.Exported()
}

// LogExporter keeps every log record exported to it, in memory (the SDK has no in-memory log
// exporter of its own). For telemetry.Config's LogExporter too.
type LogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

var _ sdklog.Exporter = (*LogExporter)(nil)

// Export keeps a copy of each record: an exporter must not retain the slice it is passed, which
// shares state with the SDK's records.
func (e *LogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}

func (e *LogExporter) Shutdown(context.Context) error { return nil }

func (e *LogExporter) ForceFlush(context.Context) error { return nil }

// Exported is what has been exported so far.
func (e *LogExporter) Exported() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.records)
}

// LogAttributes is a log record's attributes as a map, for comparing.
func LogAttributes(record sdklog.Record) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value)
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		m[kv.Key] = kv.Value
		return true
	})
	return m
}

// Spans are the spans ended so far, in the order they ended, as exported.
func (r *Recorder) Spans() []sdktrace.ReadOnlySpan {
	return r.spans.GetSpans().Snapshots()
}

// Reset forgets the spans ended so far.
func (r *Recorder) Reset() {
	r.spans.Reset()
}

// Metrics collects the metrics recorded so far.
func (r *Recorder) Metrics(t testing.TB) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

// Attributes is a span's attributes as a map, for comparing.
func Attributes(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value)
	for _, kv := range span.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}
