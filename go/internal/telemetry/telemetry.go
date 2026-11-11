// Package telemetry sets up OpenTelemetry for a process (slice 5): the tracer, meter and logger
// providers, exporting over OTLP, the W3C propagators and the SDK's error handler, registered
// globally, so the instrumentation elsewhere (otelhttp in package api, the database tracer in package
// db, package logging's copy of each log line) records through them, by the rules of
// contracts/telemetry.md. A foundation, beside logging (decision 57). Node:
// node/src/shared/telemetry.ts.
//
// The logger provider is the log lines' copy to the backend (decision 93), there unless
// OTEL_LOGS_EXPORTER is none: stdout has every line either way.
//
// Off unless the endpoint is set: nothing registered, so the global providers stay no-ops and
// nothing is exported or logged. Set, it exports in the background in batches (decision 79): an
// absent or slow backend costs telemetry, never a request or a delivery.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// DefaultShutdownGrace is how long stop may take to export what's left, unless Config says
// otherwise: within every process's grace period (the API has 5 s in all), as Node's 2 s.
const DefaultShutdownGrace = 2 * time.Second

// Config is what Setup needs.
type Config struct {
	// OTEL_EXPORTER_OTLP_ENDPOINT; "" is telemetry off. The OTLP exporters read the variable
	// themselves (and OTEL_METRIC_EXPORT_INTERVAL): this only switches them on.
	Endpoint string
	// The resource's service.name: the process's role (api, relay, deposit-worker, submitter), the
	// same in both implementations (decision 90).
	Service string
	// Where the SDK's errors go (rate-limited), and stop's if it can't flush.
	Logger *slog.Logger
	// How long stop may take; DefaultShutdownGrace if zero.
	ShutdownGrace time.Duration
	// OTEL_LOGS_EXPORTER, as config parses it: "otlp" (stdout and a copy to the backend)
	// or "none" (stdout only). Setup refuses anything else.
	Logs string

	// For tests, all three or none: used in place of OTLP whatever Endpoint says (as Node's
	// startTelemetry options).
	SpanExporter sdktrace.SpanExporter
	MetricReader sdkmetric.Reader
	LogExporter  sdklog.Exporter
}

// Setup registers the providers, propagators and error handler as cfg says, and returns stop, which
// the caller defers: once the process's work has stopped (so its last spans are in), stop exports
// what's left, within cfg.ShutdownGrace from when it is called, and logs (never returns) a failure,
// as Node's shutdownTelemetry. With telemetry off, Setup registers nothing and stop does nothing.
// An error is a startup failure: a broken resource or exporter configuration.
//
// ctx bounds Setup itself (a SIGTERM during startup ends it, as it does db.Connect); nothing keeps
// it afterwards: the batch processor and the metric reader export on contexts of their own.
func Setup(ctx context.Context, cfg Config) (stop func(), err error) {
	// Telemetry off: nothing registered, and no other check applies (a Logs of "" included).
	if cfg.Endpoint == "" && cfg.SpanExporter == nil && cfg.MetricReader == nil && cfg.LogExporter == nil {
		return func() {}, nil
	}
	// All exporters or none (for tests): with only some, the others would be real OTLP exporters.
	if (cfg.SpanExporter == nil) != (cfg.MetricReader == nil) ||
		(cfg.MetricReader == nil) != (cfg.LogExporter == nil) {
		return nil, errors.New("telemetry: all exporters or none")
	}
	// config gives only "otlp" or "none": anything else is a main that doesn't pass it.
	if cfg.Logs != "otlp" && cfg.Logs != "none" {
		return nil, fmt.Errorf("telemetry: Logs %q, want otlp or none", cfg.Logs)
	}
	// Exactly the contract's resource, plus the SDK's own attributes (telemetry.sdk.*): no host or
	// process detectors, as Node has none. service.instance.id, a random UUID for this process
	// start, tells two instances of one service apart (they would otherwise write identical metric
	// series); host.name lets a person find the instance's container (decision 90).
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.Service),
		attribute.String("implementation", "go"),
		semconv.ServiceInstanceID(uuid.NewString()),
	}
	// Node's os.hostname() can't fail; if Go's does, the attribute is left out rather than failing
	// the process's startup over it.
	if host, err := os.Hostname(); err == nil {
		attrs = append(attrs, semconv.HostName(host))
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}
	exporter := cfg.SpanExporter
	if exporter == nil {
		if exporter, err = otlptracehttp.New(ctx); err != nil {
			return nil, err
		}
	}
	// Any later failure releases the exporter: this reads Setup's named result, err, which every
	// return statement sets before deferred functions run.
	defer func() {
		if err != nil {
			_ = exporter.Shutdown(context.Background())
		}
	}()

	// Made even when Logs is "none" (then unused: an HTTP client, no goroutines), so the release
	// below never meets a nil exporter: calling a method on a nil interface panics.
	logExporter := cfg.LogExporter
	if logExporter == nil {
		if logExporter, err = otlploghttp.New(ctx); err != nil {
			return nil, err
		}
	}
	// Released on a later failure, as the span exporter is.
	defer func() {
		if err != nil {
			_ = logExporter.Shutdown(context.Background())
		}
	}()

	reader := cfg.MetricReader
	if reader == nil {
		metricExporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, err
		}
		reader = sdkmetric.NewPeriodicReader(metricExporter)
	}

	traces := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(WithoutPersonalData(exporter)),
	)
	meters := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(ContractMetrics),
	)
	// The log lines' copy: ContractLogs first, reshaping each record for the batch processor after
	// it (the SDK runs processors in the order they're registered). Registered with the others, once
	// nothing can fail.
	var logs *sdklog.LoggerProvider
	if cfg.Logs == "otlp" {
		logs = sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(ContractLogs{}),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		)
		otel.SetLoggerProvider(logs)
	}
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(meters)
	// W3C Trace Context and Baggage (decision 80), so a Node producer and a Go consumer understand
	// each other. The API's otelhttp handler has none of its own: it reads no incoming context.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	otel.SetErrorHandler(RateLimitedErrorHandler(cfg.Logger, time.Minute, time.Now))

	grace := cfg.ShutdownGrace
	if grace == 0 {
		grace = DefaultShutdownGrace
	}
	stop = func() {
		// The deadline from now, when stop is called: a context made in Setup would have expired
		// long before.
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		results := make([]error, 3)
		var wg sync.WaitGroup
		wg.Go(func() {
			results[0] = traces.Shutdown(ctx)
		})
		wg.Go(func() {
			results[1] = meters.Shutdown(ctx)
		})
		if logs != nil {
			wg.Go(func() {
				results[2] = logs.Shutdown(ctx)
			})
		}
		wg.Wait()
		if err := errors.Join(results...); err != nil {
			cfg.Logger.Warn("telemetry not flushed; dropped", "err", err)
		}
	}
	return stop, nil
}

// ContractMetrics is the metric views: the contract's metrics and only those, as Node's
// METRIC_VIEWS. An allow-list, so an instrumentation's other instruments (otelhttp's body sizes)
// are dropped rather than exported unlisted, and a kept one has only its listed attributes (bounded
// cardinality: no raw path, no address). A new contract metric must be added here, or it's
// dropped. Exported for internal/telemetrytest, so the tests see what a process exports.
func ContractMetrics(i sdkmetric.Instrument) (sdkmetric.Stream, bool) {
	if i.Name == "http.server.request.duration" {
		return sdkmetric.Stream{
			Name:        i.Name,
			Description: i.Description,
			Unit:        i.Unit,
			AttributeFilter: attribute.NewAllowKeysFilter(
				"http.request.method", "http.route", "http.response.status_code",
			),
		}, true
	}
	if i.Name == "ledger_transitions_total" {
		return sdkmetric.Stream{
			Name:        i.Name,
			Description: i.Description,
			Unit:        i.Unit,
			AttributeFilter: attribute.NewAllowKeysFilter(
				"entity", "from", "to", "implementation",
			),
		}, true
	}
	return sdkmetric.Stream{Name: i.Name, Aggregation: sdkmetric.AggregationDrop{}}, true
}

// RateLimitedErrorHandler logs the SDK's errors (mostly an export that failed: the backend absent
// or slow) without flooding: the first at once, then at most one line per interval, with how many
// were left out since the last ("suppressed"), as Node's rateLimitedErrorHandler. Safe for
// concurrent use: the SDK reports from its own goroutines.
func RateLimitedErrorHandler(
	logger *slog.Logger,
	interval time.Duration,
	now func() time.Time,
) otel.ErrorHandler {
	var mu sync.Mutex
	var last time.Time
	suppressed := 0
	return otel.ErrorHandlerFunc(func(err error) {
		// One at a time, so the count is exact.
		mu.Lock()
		defer mu.Unlock()
		at := now()
		if !last.IsZero() && at.Sub(last) < interval {
			suppressed++
			return
		}
		logger.Warn("telemetry export failed", "err", err, "suppressed", suppressed)
		last, suppressed = at, 0
	})
}
