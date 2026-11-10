// Package telemetrytest records the spans and metrics that the code under test makes, in memory: the
// Go counterpart of Node's InMemorySpanExporter and InMemoryMetricExporter in test/telemetry/.
// Imported only by tests, like testdb and relaytest.
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
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
}

// Record sets the global tracer and meter providers to ones recording in memory, and the W3C
// propagators, as telemetry.Setup sets the real ones in a process, and sets no-op ones back when
// the test ends.
// Global, so the code records through the providers it finds at run time (otelhttp, the database
// tracer); safe because no test in this module calls t.Parallel, so a package's tests run one at a
// time. Build whatever captures a provider (an otelhttp handler) after calling it.
//
// Spans are exported as they end (WithSyncer), through the same filter as Setup's exporter
// (telemetry.WithoutPersonalData), so a test sees what a process exports.
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
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(meters)
	// The W3C propagators, as Setup registers them: the outbox and the relay write trace context
	// through the global propagator, which is a no-op until one is set.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Cleanup(func() {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
		_ = traces.Shutdown(context.Background())
		_ = meters.Shutdown(context.Background())
	})
	return &Recorder{spans: spans, reader: reader}
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
