package telemetry

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// personalAttributes are attributes the HTTP conventions record that the contract keeps out of every
// span (its privacy rule): the client's IP addresses, personal data under the GDPR (otelhttp takes
// client.address from X-Forwarded-For when there is one), and its user agent, nearly so. otelhttp
// has no option to leave them out, and an attribute can't be removed from a span, so they are left
// out at export. Node never sets them (its HTTP instrumentation's start hook).
var personalAttributes = []attribute.Key{
	"client.address",
	"network.peer.address",
	"user_agent.original",
}

// WithoutPersonalData wraps exporter so that no span it exports carries personalAttributes. Setup
// wraps every exporter in it, the tests' too, as does telemetrytest, so the API's tests see what is
// exported.
func WithoutPersonalData(exporter sdktrace.SpanExporter) sdktrace.SpanExporter {
	return withoutPersonalData{exporter}
}

type withoutPersonalData struct {
	sdktrace.SpanExporter
}

// ExportSpans exports each span as a copy without personalAttributes. Shutdown comes with the
// embedded exporter.
func (e withoutPersonalData) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	redacted := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		redacted[i] = redactedSpan{span}
	}
	return e.SpanExporter.ExportSpans(ctx, redacted)
}

// redactedSpan is a span with personalAttributes left out of Attributes. Embedding the interface
// makes it one: every other method is the span's own, its unexported one included (ReadOnlySpan has
// one, so only the SDK's types and embeddings like this can satisfy it).
type redactedSpan struct {
	sdktrace.ReadOnlySpan
}

func (s redactedSpan) Attributes() []attribute.KeyValue {
	return slices.DeleteFunc(slices.Clone(s.ReadOnlySpan.Attributes()), func(kv attribute.KeyValue) bool {
		return slices.Contains(personalAttributes, kv.Key)
	})
}
