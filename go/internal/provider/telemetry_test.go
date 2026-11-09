package provider_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/guywillett/ledger-demo/go/internal/submitter"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
)

// Decision 84: the provider is outside the system, so the submitter's calls carry no trace headers,
// even from inside a span (the processing span, from step 2). The client is a plain http.Client, not
// otelhttp's transport, so nothing injects them. Node: workers.telemetry.test.ts.
func TestProviderCallsCarryNoTraceHeadersEvenFromInsideASpan(t *testing.T) {
	telemetrytest.Record(t)
	var received []http.Header
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"transfer_id":"tr_1","status":"accepted"}`)
	})
	ctx, span := otel.Tracer("test").Start(t.Context(), "a delivery")
	outcome := client(t, url, 2*time.Second).SubmitTransfer(ctx, withdrawal())
	span.End()

	if outcome.Kind != submitter.Accepted || len(received) != 1 {
		t.Fatalf("outcome %+v, %d requests", outcome, len(received))
	}
	for _, header := range []string{"Traceparent", "Tracestate", "Baggage"} {
		if v := received[0].Get(header); v != "" {
			t.Errorf("%s: %q", header, v)
		}
	}
}
