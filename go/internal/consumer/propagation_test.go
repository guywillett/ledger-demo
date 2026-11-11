package consumer_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The processing span (slice 5 decisions 80-82): the cases of
// node/test/telemetry/propagation.telemetry.test.ts that are the consumer layer's, through
// Lifecycle.Deliver with a fake message. The relay's half (the publish span, the headers it sends)
// is internal/relay's telemetry tests.

// published is a publish span's context and the headers that carry it, as the relay sends them.
func published(t *testing.T) (trace.SpanContext, nats.Header) {
	t.Helper()
	_, span := otel.Tracer("test").Start(context.Background(), "publish test.event",
		trace.WithSpanKind(trace.SpanKindProducer))
	span.End()
	sc := span.SpanContext()
	return sc, nats.Header{
		"traceparent": {"00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"},
	}
}

// depositBody is a test event that also names a deposit, as deposit.created does.
func depositBody(t *testing.T) (string, ids.EventID, ids.DepositID) {
	t.Helper()
	eventID, deposit := ids.New[ids.Event](), ids.New[ids.Deposit]()
	data, err := json.Marshal(map[string]any{
		"event_id": eventID.TypeID(), "deposit_id": deposit.TypeID(), "n": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data), eventID, deposit
}

func processSpans(rec *telemetrytest.Recorder) []sdktrace.ReadOnlySpan {
	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.Spans() {
		if s.Name() == "process test.event" {
			found = append(found, s)
		}
	}
	return found
}

func linkedTo(span sdktrace.ReadOnlySpan) []trace.SpanContext {
	var found []trace.SpanContext
	for _, l := range span.Links() {
		found = append(found, l.SpanContext)
	}
	return found
}

func sameSpan(a, b trace.SpanContext) bool {
	return a.TraceID() == b.TraceID() && a.SpanID() == b.SpanID()
}

// done is a handler that settles at once, noting the context it ran in.
func done(seen *trace.SpanContext) consumer.Handler[testEvent] {
	return func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
		*seen = trace.SpanContextFromContext(ctx)
		d.Log().Info("handled")
		return d.Done(ctx, consumer.Applied)
	}
}

func TestADeliveryIsARootProcessingSpanLinkedToItsPublish(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	publish, header := published(t)
	body, eventID, deposit := depositBody(t)
	m := message(body)
	m.header = header
	logs, logger := newCapture(t)
	// A span in ctx is not the processing span's parent: it is a root of its own.
	ctx, caller := otel.Tracer("test").Start(context.Background(), "the consume loop")
	defer caller.End()
	var seen trace.SpanContext

	outcome := lifecycle(pool, logger).Deliver(ctx, m, done(&seen))

	spans := processSpans(rec)
	if outcome != consumer.Applied || len(spans) != 1 {
		t.Fatalf("outcome %s, %d processing spans", outcome, len(spans))
	}
	process := spans[0]
	if process.SpanKind() != trace.SpanKindConsumer || process.Parent().IsValid() {
		t.Errorf("kind %v, parent %v; want a CONSUMER root", process.SpanKind(), process.Parent())
	}
	if links := linkedTo(process); len(links) != 1 || !sameSpan(links[0], publish) {
		t.Errorf("links %v, want the publish %v", links, publish)
	}
	attrs := telemetrytest.Attributes(process)
	for key, want := range map[attribute.Key]string{
		"messaging.system":              "nats",
		"messaging.operation.name":      "process",
		"messaging.operation.type":      "process",
		"messaging.destination.name":    "test.event",
		"messaging.consumer.group.name": "deposit-worker",
		"messaging.message.id":          m.msgID,
		"event_id":                      eventID.TypeID(),
		"deposit_id":                    deposit.TypeID(),
		"delivery_outcome":              "applied",
	} {
		if got := attrs[key].AsString(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := attrs["delivery_attempt"].AsInt64(); got != 1 {
		t.Errorf("delivery_attempt = %d, want 1", got)
	}

	// The delivery runs in it: the handler's context, the delivery log's statements, the log lines.
	if !sameSpan(seen, process.SpanContext()) {
		t.Errorf("the handler ran in %v, want the processing span %v", seen, process.SpanContext())
	}
	var statements []string
	for _, s := range rec.Spans() {
		if s.SpanKind() == trace.SpanKindClient && s.Parent().SpanID() == process.SpanContext().SpanID() {
			statements = append(statements, s.Name())
		}
	}
	if !slices.Equal(statements, []string{"INSERT", "UPDATE"}) { // the delivery log's
		t.Errorf("the processing span's statements %v, want the delivery log's INSERT and UPDATE", statements)
	}
	line := logs.one(t, "handled")
	if line["trace_id"] != process.SpanContext().TraceID().String() ||
		line["span_id"] != process.SpanContext().SpanID().String() {
		t.Errorf("line %v, want the processing span's IDs", line)
	}
}

// Decision 82: JetStream stores the headers with the message, so a redelivery carries the same
// context: a span of its own, linked to the same publish, with its own attempt.
func TestARedeliveryIsAProcessingSpanOfItsOwnLinkedToTheSamePublish(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	publish, header := published(t)
	body, _, _ := depositBody(t)
	for attempt := 1; attempt <= 2; attempt++ {
		m := message(body)
		m.header, m.attempt = header, attempt
		var seen trace.SpanContext
		lifecycle(pool, discard()).Deliver(context.Background(), m, done(&seen))
	}
	spans := processSpans(rec)
	if len(spans) != 2 {
		t.Fatalf("%d processing spans, want 2", len(spans))
	}
	for i, s := range spans {
		if got := telemetrytest.Attributes(s)["delivery_attempt"].AsInt64(); got != int64(i+1) {
			t.Errorf("span %d: delivery_attempt %d", i, got)
		}
		if links := linkedTo(s); len(links) != 1 || !sameSpan(links[0], publish) {
			t.Errorf("span %d: links %v, want the publish", i, links)
		}
	}
	if spans[0].SpanContext().TraceID() == spans[1].SpanContext().TraceID() {
		t.Error("the redelivery is in the first delivery's trace")
	}
}

func TestADeliveryWithoutTraceContextIsProcessedNormallyUnlinked(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	body, _, _ := depositBody(t)
	var seen trace.SpanContext
	if outcome := lifecycle(pool, discard()).Deliver(context.Background(), message(body), done(&seen)); outcome != consumer.Applied {
		t.Fatalf("outcome %s", outcome)
	}
	spans := processSpans(rec)
	if len(spans) != 1 || len(spans[0].Links()) != 0 {
		t.Errorf("spans %v, want one, unlinked", spans)
	}
}

// Decision 83: a malformed carrier changes nothing but the trace.
func TestAMalformedTraceparentIsProcessedNormallyNoLinkNoError(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	body, _, _ := depositBody(t)
	m := message(body)
	m.header = nats.Header{"traceparent": {"not-a-traceparent"}}
	var seen trace.SpanContext
	if outcome := lifecycle(pool, discard()).Deliver(context.Background(), m, done(&seen)); outcome != consumer.Applied {
		t.Fatalf("outcome %s", outcome)
	}
	spans := processSpans(rec)
	if len(spans) != 1 || len(spans[0].Links()) != 0 || spans[0].Status().Code == codes.Error {
		t.Errorf("spans %v, want one, unlinked, no error", spans)
	}
}

// The contract: an invalid or failed delivery is an error on its span.
func TestAnInvalidDeliveryIsAnErrorOnItsSpan(t *testing.T) {
	rec := telemetrytest.Record(t)
	pool := testdb.Pool(t)
	var seen trace.SpanContext
	outcome := lifecycle(pool, discard()).Deliver(context.Background(), message(`{"not":"an event"}`), done(&seen))
	spans := processSpans(rec)
	if outcome != consumer.Invalid || len(spans) != 1 {
		t.Fatalf("outcome %s, %d spans", outcome, len(spans))
	}
	if got := telemetrytest.Attributes(spans[0])["delivery_outcome"].AsString(); got != "invalid" ||
		spans[0].Status().Code != codes.Error || spans[0].Status().Description != "delivery invalid" {
		t.Errorf("delivery_outcome %q, status %+v", got, spans[0].Status())
	}
}
