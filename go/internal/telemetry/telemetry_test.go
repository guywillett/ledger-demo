package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/telemetry"
)

// Setup and its shutdown, with in-memory exporters: the cases of Node's startTelemetry tests
// (node/test/shared/telemetry.test.ts, node/test/telemetry/). Setup sets global providers, so each
// test sets no-op ones back when it ends (restoreGlobals).

// recordingExporter keeps every span exported to it. Not tracetest.InMemoryExporter: its Shutdown
// forgets what it holds, and these tests look after shutdown.
type recordingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error { return nil }

func (e *recordingExporter) exported() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.spans)
}

// blockingExporter never finishes an export until its context ends: a backend that doesn't answer.
type blockingExporter struct{}

func (blockingExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	<-ctx.Done()
	return ctx.Err()
}

func (blockingExporter) Shutdown(context.Context) error { return nil }

// logs is a logger whose lines the test reads back.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) lines(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		found = append(found, m)
	}
	return found
}

func newLogger(t *testing.T) (*logs, *slog.Logger) {
	t.Helper()
	l := &logs{}
	logger, err := logging.New(l, "api", "info")
	if err != nil {
		t.Fatal(err)
	}
	return l, logger
}

func restoreGlobals(t *testing.T) {
	t.Cleanup(func() {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	})
}

// setup runs Setup with cfg (Service and Logger filled in if unset) and fails the test on an error.
func setup(t *testing.T, cfg telemetry.Config) (stop func()) {
	t.Helper()
	restoreGlobals(t)
	if cfg.Service == "" {
		cfg.Service = "api"
	}
	if cfg.Logger == nil {
		_, cfg.Logger = newLogger(t)
	}
	// A test's exporters come in pairs: with only a span exporter, Setup would make a real OTLP
	// metric exporter, failing at shutdown with nothing to send to.
	if cfg.SpanExporter != nil && cfg.MetricReader == nil {
		cfg.MetricReader = sdkmetric.NewManualReader()
	}
	stop, err := telemetry.Setup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return stop
}

func TestSetupIsANoOpWithoutAnEndpoint(t *testing.T) {
	l, logger := newLogger(t)
	stop := setup(t, telemetry.Config{Logger: logger})
	_, span := otel.Tracer("test").Start(context.Background(), "nothing")
	if span.IsRecording() {
		t.Error("a span records with telemetry off")
	}
	span.End()
	stop()
	if lines := l.lines(t); len(lines) != 0 {
		t.Errorf("lines %v, want none", lines)
	}
}

// Batched (decision 79), so exported in the background: by stop at the latest, which flushes.
func TestASpanStartedAfterSetupReachesTheExporterByStop(t *testing.T) {
	exporter := &recordingExporter{}
	stop := setup(t, telemetry.Config{SpanExporter: exporter})
	_, span := otel.Tracer("test").Start(context.Background(), "a span")
	span.End()
	stop()
	if got := exporter.exported(); len(got) != 1 || got[0].Name() != "a span" {
		t.Errorf("exported %v, want the one span", got)
	}
}

// Decision 90: exactly the contract's resource, as Node's (no host or process detectors). Go's SDK
// also merges OTEL_RESOURCE_ATTRIBUTES whatever Setup does (sdktrace.WithResource adds the
// environment's); compose sets none, as Node reads none.
func TestTheResourceIsTheRoleTheImplementationAndTheSDKsOwnAttributes(t *testing.T) {
	exporter := &recordingExporter{}
	stop := setup(t, telemetry.Config{Service: "relay", SpanExporter: exporter})
	_, span := otel.Tracer("test").Start(context.Background(), "a span")
	span.End()
	stop()
	spans := exporter.exported()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	got := map[string]string{}
	for _, kv := range spans[0].Resource().Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got["service.name"] != "relay" || got["implementation"] != "go" || got["telemetry.sdk.language"] != "go" {
		t.Errorf("resource %v", got)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"host.name", "implementation", "service.instance.id", "service.name",
		"telemetry.sdk.language", "telemetry.sdk.name", "telemetry.sdk.version"}
	if !slices.Equal(keys, want) {
		t.Errorf("resource keys %v\nwant          %v", keys, want)
	}
}

// The contract's privacy rule: what the HTTP conventions record about the client never leaves the
// process, and nothing else is touched.
func TestExportedSpansCarryNoClientAddressOrUserAgent(t *testing.T) {
	exporter := &recordingExporter{}
	stop := setup(t, telemetry.Config{SpanExporter: exporter})
	_, span := otel.Tracer("test").Start(context.Background(), "a request")
	span.SetAttributes(
		attribute.String("client.address", "203.0.113.7"),
		attribute.String("network.peer.address", "203.0.113.7"),
		attribute.String("user_agent.original", "Mozilla/5.0"),
		attribute.String("url.path", "/health"),
	)
	span.End()
	stop()
	spans := exporter.exported()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	var keys []string
	for _, kv := range spans[0].Attributes() {
		keys = append(keys, string(kv.Key))
	}
	if !slices.Equal(keys, []string{"url.path"}) {
		t.Errorf("attributes %v, want only url.path", keys)
	}
	if spans[0].Name() != "a request" {
		t.Errorf("name %q", spans[0].Name())
	}
}

// Decision 90: two instances of one service are told apart (identical metric series otherwise), and
// a person can find each one's container.
func TestEachSetupHasItsOwnInstanceIDAndTheHostName(t *testing.T) {
	resourceOf := func() map[string]string {
		exporter := &recordingExporter{}
		stop := setup(t, telemetry.Config{Service: "deposit-worker", SpanExporter: exporter})
		_, span := otel.Tracer("test").Start(context.Background(), "a span")
		span.End()
		stop()
		spans := exporter.exported()
		if len(spans) != 1 {
			t.Fatalf("%d spans", len(spans))
		}
		got := map[string]string{}
		for _, kv := range spans[0].Resource().Attributes() {
			got[string(kv.Key)] = kv.Value.String()
		}
		return got
	}
	first, second := resourceOf(), resourceOf()
	if err := uuid.Validate(first["service.instance.id"]); err != nil {
		t.Errorf("service.instance.id %q: %v", first["service.instance.id"], err)
	}
	if first["service.instance.id"] == second["service.instance.id"] {
		t.Errorf("both setups have service.instance.id %s", first["service.instance.id"])
	}
	if host, _ := os.Hostname(); first["host.name"] != host {
		t.Errorf("host.name %q, want %q", first["host.name"], host)
	}
}

// Decision 80: W3C Trace Context and Baggage, so a Node producer and a Go consumer understand each
// other (step 2).
func TestThePropagatorsAreW3CTraceContextAndBaggage(t *testing.T) {
	setup(t, telemetry.Config{SpanExporter: &recordingExporter{}})
	fields := otel.GetTextMapPropagator().Fields()
	slices.Sort(fields)
	if want := []string{"baggage", "traceparent", "tracestate"}; !slices.Equal(fields, want) {
		t.Errorf("fields %v, want %v", fields, want)
	}
}

// The contract's metrics only, each with only its listed attributes: otelhttp's body sizes are
// dropped, and the duration keeps method, route and status (no address, no raw path).
func TestOnlyTheContractsMetricsAreExportedWithTheirListedAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	setup(t, telemetry.Config{SpanExporter: &recordingExporter{}, MetricReader: reader})
	// As the API's handler: otelhttp, the route through its labeler (internal/api's nameByRoute).
	handler := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
			labeler.Add(attribute.String("http.route", "/accounts/{account_id}/balance"))
		}
		_, _ = w.Write([]byte(`{}`))
	}), "")
	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/accounts/acct_x/balance", nil))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, point := range histogram.DataPoints {
				var keys []string
				for _, kv := range point.Attributes.ToSlice() {
					keys = append(keys, string(kv.Key))
				}
				slices.Sort(keys)
				if want := []string{"http.request.method", "http.response.status_code", "http.route"}; !slices.Equal(keys, want) {
					t.Errorf("%s attributes %v, want %v", m.Name, keys, want)
				}
			}
		}
	}
	if want := []string{"http.server.request.duration"}; !slices.Equal(names, want) {
		t.Errorf("metrics %v, want %v", names, want)
	}
}

// Decision 79: an exporter's failure is logged, not raised, and not a line per failure.
func TestTheSDKsErrorsGoToTheLoggerWithoutFlooding(t *testing.T) {
	l, logger := newLogger(t)
	setup(t, telemetry.Config{SpanExporter: &recordingExporter{}, Logger: logger})
	for range 3 {
		otel.Handle(errors.New("connect: connection refused"))
	}
	lines := l.lines(t)
	if len(lines) != 1 || lines[0]["msg"] != "telemetry export failed" || lines[0]["level"] != "warn" {
		t.Errorf("lines %v, want one warning", lines)
	}
}

func TestRateLimitedErrorHandlerLogsTheFirstAtOnceThenOnePerIntervalCountingTheRest(t *testing.T) {
	l, logger := newLogger(t)
	now := time.Unix(0, 0)
	handler := telemetry.RateLimitedErrorHandler(logger, time.Minute, func() time.Time { return now })

	handler.Handle(errors.New("connect ECONNREFUSED"))
	now = now.Add(time.Second)
	handler.Handle(errors.New("again"))
	handler.Handle(errors.New("and again"))
	now = now.Add(time.Minute)
	handler.Handle(errors.New("a minute on"))

	var got [][3]any
	for _, line := range l.lines(t) {
		err, _ := line["err"].(map[string]any) // the logging package writes an error as pino does
		got = append(got, [3]any{line["msg"], line["suppressed"], err["message"]})
	}
	want := [][3]any{
		{"telemetry export failed", float64(0), "connect ECONNREFUSED"},
		{"telemetry export failed", float64(2), "a minute on"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("lines %v\nwant  %v", got, want)
	}
}

// A backend that doesn't answer can't hold stop beyond its grace (within every process's grace
// period, as Node's 2 s bound): what's left is dropped, with one warning.
func TestStopGivesUpAfterItsGraceAndSaysSo(t *testing.T) {
	l, logger := newLogger(t)
	stop := setup(t, telemetry.Config{
		SpanExporter:  blockingExporter{},
		Logger:        logger,
		ShutdownGrace: 200 * time.Millisecond,
	})
	_, span := otel.Tracer("test").Start(context.Background(), "never exported")
	span.End()
	start := time.Now()
	stop()
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stop took %v", took)
	}
	lines := l.lines(t)
	if len(lines) != 1 || lines[0]["msg"] != "telemetry not flushed; dropped" || lines[0]["level"] != "warn" {
		t.Errorf("lines %v, want one warning", lines)
	}
}
