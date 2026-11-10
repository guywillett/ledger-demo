package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Trace context as a string map, the carrier the propagators read and write (W3C: traceparent,
// tracestate, baggage), for crossing the outbox and NATS (slice 5 decisions 80-82): stored in the
// outbox's trace_context by the API, restored by the relay, sent as NATS headers to the consumers.
// Observation only: nothing reads it to decide anything (decision 83), and a missing or malformed
// one changes nothing but the trace. Through the global propagator, so with telemetry off (no span
// in any context) nothing is written. Node: node/src/shared/trace-context.ts.

// Carrier is ctx's span context as a carrier; nil outside a traced context (telemetry off, tests).
func Carrier(ctx context.Context) map[string]string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return nil
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// LinksTo is a link to the context in carrier, for a span that starts its own trace but was caused
// by it (the messaging conventions: the relay's publish to the request, a consumer's processing to
// the publish): none when carrier is nil, empty or malformed. Any carrier: a stored map
// (propagation.MapCarrier) or a message's headers.
func LinksTo(carrier propagation.TextMapCarrier) []trace.Link {
	if carrier == nil {
		return nil
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return nil
	}
	return []trace.Link{{SpanContext: spanContext}}
}
