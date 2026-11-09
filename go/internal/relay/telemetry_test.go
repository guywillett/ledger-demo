package relay_test

import (
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/relay"
	"github.com/guywillett/ledger-demo/go/internal/relaytest"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
)

// Decision 85: a claim that finds nothing, and its query, are not traced (the query runs outside any
// span, so the database tracer makes none). Node: workers.telemetry.test.ts.
func TestAnIdleRelayMakesNoSpans(t *testing.T) {
	e := setUp(t) // the outbox drained
	rec := telemetrytest.Record(t)
	_, logger := newCapture(t)
	for range 3 {
		batch, err := relay.PublishBatch(t.Context(), relaytest.Deps(e.pool, e.stream, logger))
		if err != nil || batch.Claimed != 0 {
			t.Fatalf("batch %+v, %v; want nothing claimed", batch, err)
		}
	}
	if spans := rec.Spans(); len(spans) != 0 {
		t.Errorf("%d spans, want none", len(spans))
	}
}
