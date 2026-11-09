package consumer_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The consumers' telemetry: the cases of node/test/telemetry/workers.telemetry.test.ts that are the
// consume loop's and the delivery's.

// Decision 85: a consumer that waits makes nothing to trace.
func TestAnIdleConsumerMakesNoSpansThroughItsEmptyFetches(t *testing.T) {
	rec := telemetrytest.Record(t)
	tc := newTestConsumer(t, 30*time.Second)
	c := tc.start(t, consumer.ConsumeOptions{
		Concurrency:  1,
		FetchExpires: 200 * time.Millisecond, // several empty fetches in the time below
		Handle: func(context.Context, consumer.Message) {
			t.Error("no message was expected")
		},
	})
	time.Sleep(time.Second)
	c.Stop()
	if spans := rec.Spans(); len(spans) != 0 {
		t.Errorf("%d spans, want none: %v", len(spans), spans)
	}
}

// Decision 86: the delivery's logger carries the trace_id and span_id of the span it runs in (the
// processing span, from step 2), on its own lines and the handler's.
func TestADeliveryInASpanLogsItsTraceIDs(t *testing.T) {
	telemetrytest.Record(t)
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	logs, logger := newCapture(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "a delivery")
	defer span.End()

	lifecycle(pool, logger).Deliver(ctx, message(body),
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			d.Log().Info("handled")
			return d.Done(ctx, consumer.Applied)
		})

	line := logs.one(t, "handled")
	if line["trace_id"] != span.SpanContext().TraceID().String() ||
		line["span_id"] != span.SpanContext().SpanID().String() {
		t.Errorf("line %v, span %v", line, span.SpanContext())
	}
}

func TestADeliveryOutsideASpanLogsNoTraceIDs(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	logs, logger := newCapture(t)
	lifecycle(pool, logger).Deliver(context.Background(), message(body),
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			d.Log().Info("handled")
			return d.Done(ctx, consumer.Applied)
		})
	if line := logs.one(t, "handled"); line["trace_id"] != nil || line["span_id"] != nil {
		t.Errorf("trace fields on %v", line)
	}
}
