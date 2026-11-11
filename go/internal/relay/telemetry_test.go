package relay_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ingestion"
	"github.com/guywillett/ledger-demo/go/internal/relay"
	"github.com/guywillett/ledger-demo/go/internal/relaytest"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
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

// Trace context through the outbox (slice 5 decisions 80-83): the cases of
// node/test/telemetry/propagation.telemetry.test.ts that are the relay's. The consumer's half is
// internal/consumer's propagation tests.

// newDepositIn records a deposit as newDeposit does, in ctx: inside a span, as the API's request.
func newDepositIn(ctx context.Context, t *testing.T, pool *pgxpool.Pool) ids.DepositID {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	var result ingestion.Result
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		var err error
		result, err = ingestion.RecordNotification(ctx, tx, ingestion.Notification{
			CreditorIBAN:         sepa.IBAN(account.IBAN),
			Amount:               1250,
			Currency:             "EUR",
			Network:              "SEPA",
			NetworkTransactionID: "go-relay-test-" + ids.New[ids.Event]().TypeID(),
		})
		return err
	})
	if err != nil || result.Kind != ingestion.Created {
		t.Fatalf("recording a deposit: %v %v", result.Kind, err)
	}
	return result.Deposit
}

func spansNamed(rec *telemetrytest.Recorder, name string) []sdktrace.ReadOnlySpan {
	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.Spans() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	return found
}

// publishOne publishes the outbox (holding only deposit's event after setUp's drain) and returns
// what was published for deposit.
func publishOne(t *testing.T, e env, deposit ids.DepositID) relaytest.Published {
	t.Helper()
	recording := relaytest.NewRecording(t, e.stream)
	if _, err := relay.PublishBatch(t.Context(), relaytest.Deps(e.pool, recording, nil)); err != nil {
		t.Fatal(err)
	}
	ours := recording.WithMsgID(msgIDOf(deposit))
	if len(ours) != 1 {
		t.Fatalf("%d publishes of the deposit's event, want 1", len(ours))
	}
	return ours[0]
}

func TestAPublishIsInTheBatchsTraceLinkedToTheRequestAndItsContextIsTheMessagesHeaders(t *testing.T) {
	e := setUp(t)
	rec := telemetrytest.Record(t)
	ctx, request := otel.Tracer("test").Start(context.Background(), "a request")
	deposit := newDepositIn(ctx, t, e.pool)
	request.End()
	rec.Reset()

	published := publishOne(t, e, deposit)

	batches := spansNamed(rec, "relay batch")
	publishes := spansNamed(rec, "publish deposit.created")
	if len(batches) != 1 || len(publishes) != 1 {
		t.Fatalf("%d batches, %d publishes", len(batches), len(publishes))
	}
	batch, publish := batches[0], publishes[0]
	if batch.Parent().IsValid() || batch.SpanContext().TraceID() == request.SpanContext().TraceID() {
		t.Errorf("batch: parent %v, trace %v; want a root of its own", batch.Parent(), batch.SpanContext().TraceID())
	}
	if publish.SpanKind() != trace.SpanKindProducer || publish.Parent().SpanID() != batch.SpanContext().SpanID() {
		t.Errorf("publish: kind %v, parent %v", publish.SpanKind(), publish.Parent())
	}
	links := publish.Links()
	if len(links) != 1 || links[0].SpanContext.TraceID() != request.SpanContext().TraceID() ||
		links[0].SpanContext.SpanID() != request.SpanContext().SpanID() {
		t.Errorf("links %v, want the request %v", links, request.SpanContext())
	}
	attrs := telemetrytest.Attributes(publish)
	for key, want := range map[attribute.Key]string{
		"messaging.system":           "nats",
		"messaging.operation.name":   "publish",
		"messaging.operation.type":   "send",
		"messaging.destination.name": "deposit.created",
		"messaging.message.id":       msgIDOf(deposit),
		"deposit_id":                 deposit.TypeID(),
	} {
		if got := attrs[key].AsString(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if !strings.HasPrefix(attrs["event_id"].AsString(), "evt_") {
		t.Errorf("event_id = %q", attrs["event_id"].AsString())
	}
	// The marks' statements are the batch's children (the step 1 rule: inside a span only).
	var marks []string
	for _, s := range rec.Spans() {
		if s.SpanKind() == trace.SpanKindClient && s.Parent().SpanID() == batch.SpanContext().SpanID() {
			marks = append(marks, s.Name())
		}
	}
	if !slices.Contains(marks, "UPDATE") {
		t.Errorf("batch's statements %v, want the mark's UPDATE", marks)
	}

	// The stored message: Nats-Msg-Id as ever, and the publish span's context beside it.
	stored := storedMessage(t, e.conn, broker.StreamDeposits, published.Result.Seq)
	want := "00-" + publish.SpanContext().TraceID().String() + "-" + publish.SpanContext().SpanID().String() + "-01"
	if stored.Header.Get("Nats-Msg-Id") != msgIDOf(deposit) || stored.Header.Get("traceparent") != want {
		t.Errorf("headers %v, want Nats-Msg-Id %s and traceparent %s", stored.Header, msgIDOf(deposit), want)
	}
}

func TestAnEventEnqueuedOutsideASpanStoresNoTraceContextAndPublishesUnlinked(t *testing.T) {
	e := setUp(t)
	rec := telemetrytest.Record(t)
	deposit, _ := newDeposit(t, e.pool, 1250)
	var stored *string
	if err := e.pool.QueryRow(t.Context(),
		`SELECT trace_context::text FROM outbox WHERE entity_id = $1`, deposit).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Errorf("trace_context %s, want NULL", *stored)
	}
	if published := publishOne(t, e, deposit); published.Result.Duplicate {
		t.Error("published as a duplicate")
	}
	if links := spansNamed(rec, "publish deposit.created")[0].Links(); len(links) != 0 {
		t.Errorf("links %v, want none", links)
	}
}

// Decision 83: a malformed carrier changes nothing but the trace.
func TestAMalformedStoredTraceContextIsOnlyALostLink(t *testing.T) {
	e := setUp(t)
	rec := telemetrytest.Record(t)
	deposit, _ := newDeposit(t, e.pool, 1250)
	if _, err := e.pool.Exec(t.Context(),
		`UPDATE outbox SET trace_context = '{"traceparent": "not-a-traceparent"}' WHERE entity_id = $1`,
		deposit); err != nil {
		t.Fatal(err)
	}
	publishOne(t, e, deposit)
	publish := spansNamed(rec, "publish deposit.created")[0]
	if len(publish.Links()) != 0 || publish.Status().Code == codes.Error {
		t.Errorf("links %v, status %v; want none and no error", publish.Links(), publish.Status())
	}
}

// Decision 83: Nats-Msg-Id is the outbox's msg_id whatever the trace headers.
func TestARepublishWithOtherTraceHeadersIsStillDroppedByTheStream(t *testing.T) {
	e := setUp(t)
	deposit, _ := newDeposit(t, e.pool, 1250)
	published := publishOne(t, e, deposit)
	again, err := e.stream.Publish(t.Context(), published.Subject, published.Payload, published.MsgID,
		map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	if err != nil || !again.Duplicate || again.Seq != published.Result.Seq {
		t.Errorf("republish %+v, %v; want a duplicate of seq %d", again, err, published.Result.Seq)
	}
}
