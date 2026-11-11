import {
  ROOT_CONTEXT,
  context,
  isSpanContextValid,
  propagation,
  trace,
  type Link,
} from '@opentelemetry/api';

// Trace context as a string map, the carrier the propagators read and write (W3C: traceparent,
// tracestate, baggage), for crossing the outbox and NATS (slice 5 decisions 80-82): stored in the
// outbox's trace_context by the API, restored by the relay, sent as NATS headers to the consumers.
// Observation only: nothing reads it to decide anything (decision 83), and a missing or malformed
// one changes nothing but the trace. The OpenTelemetry API only, so it costs nothing with
// telemetry off (no SDK: no span is ever active, so nothing is written). Go: internal/telemetry's
// carrier helpers.

export type TraceCarrier = Readonly<Record<string, string>>;

// The current span's context as a carrier; null outside a traced context (telemetry off, tests).
export function currentTraceCarrier(): TraceCarrier | null {
  const spanContext = trace.getSpanContext(context.active());
  if (spanContext === undefined || !isSpanContextValid(spanContext)) return null;
  const carrier: Record<string, string> = {};
  propagation.inject(context.active(), carrier);
  return Object.keys(carrier).length === 0 ? null : carrier;
}

// A link to the context in carrier, for a span that starts its own trace but was caused by it (the
// messaging conventions: the relay's publish to the request, a consumer's processing to the
// publish): none when carrier is null, empty or malformed.
export function linksTo(carrier: TraceCarrier | null): Link[] {
  if (carrier === null) return [];
  const spanContext = trace.getSpanContext(propagation.extract(ROOT_CONTEXT, carrier));
  return spanContext !== undefined && isSpanContextValid(spanContext)
    ? [{ context: spanContext }]
    : [];
}
