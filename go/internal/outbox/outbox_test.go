package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/outbox"
	"github.com/guywillett/ledger-demo/go/internal/relaytest"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The outbox's data access, the cases of node/test/outbox/outbox.test.ts, against the relay test
// database. Claim takes the oldest unpublished events in the whole table, so this package holds the
// database's lock while it runs (no relay test runs meanwhile) and drains the backlog before each
// test. Assertions still look only at each test's own events: an event backed off by an earlier
// test can become claimable again at any moment.
func TestMain(m *testing.M) {
	os.Exit(testdb.Exclusive(m, testdb.Relay))
}

// setUp drains the outbox and returns the pool. Every event the test creates is marked published
// when it ends: their payloads are not real events, so they mustn't reach the test stream.
func setUp(t *testing.T) (*pgxpool.Pool, *[]ids.EventID) {
	t.Helper()
	pool := testdb.PoolFor(t, testdb.Relay)
	conn := relaytest.Conn(t)
	relaytest.Drain(t, pool, relaytest.StreamPublisher(t, conn))
	created := &[]ids.EventID{}
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(),
			`UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, *created)
		if err != nil {
			t.Error(err)
		}
	})
	return pool, created
}

// testEvent is a minimal event about a fresh (never stored) deposit; the outbox doesn't check
// that it exists.
func testEvent(created *[]ids.EventID) outbox.NewEvent {
	id := ids.New[ids.Event]()
	deposit := ids.New[ids.Deposit]()
	*created = append(*created, id)
	return outbox.NewEvent{
		ID:      id,
		Entity:  outbox.Deposit(deposit),
		Subject: "deposit.created",
		MsgID:   deposit.TypeID() + ".created",
		Payload: map[string]any{
			"event_id": id.TypeID(),
			"nested":   map[string]any{"amount_minor": "1250"},
		},
	}
}

func depositOf(e outbox.NewEvent) ids.DepositID {
	id, _ := e.Entity.DepositID()
	return id
}

func enqueueAll(t *testing.T, pool *pgxpool.Pool, events ...outbox.NewEvent) {
	t.Helper()
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		for _, e := range events {
			if err := outbox.Enqueue(context.Background(), tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// claimOnly claims (limit 100) and keeps only the given events' IDs, in the order claimed.
func claimOnly(
	t *testing.T,
	pool *pgxpool.Pool,
	lease time.Duration,
	events ...outbox.NewEvent,
) []ids.EventID {
	t.Helper()
	claimed, err := outbox.Claim(context.Background(), pool, 100, lease)
	if err != nil {
		t.Fatal(err)
	}
	var ours []ids.EventID
	for _, c := range claimed {
		if slices.ContainsFunc(events, func(e outbox.NewEvent) bool { return e.ID == c.ID }) {
			ours = append(ours, c.ID)
		}
	}
	return ours
}

// idOrder is the events' IDs sorted: UUIDv7s sort by creation time, as the claim's ORDER BY id.
func idOrder(events ...outbox.NewEvent) []ids.EventID {
	sorted := make([]ids.EventID, len(events))
	for i, e := range events {
		sorted[i] = e.ID
	}
	slices.SortFunc(sorted, byID)
	return sorted
}

func byID(a, b ids.EventID) int {
	return strings.Compare(a.String(), b.String())
}

func TestEnqueueStoresTheEventWithItsPayloadAsJSON(t *testing.T) {
	pool, created := setUp(t)
	event := testEvent(created)
	enqueueAll(t, pool, event)
	row := relaytest.OneRow(t, pool, depositOf(event))
	if row.ID != event.ID.UUID().String() || row.EntityType != "deposit" ||
		row.Subject != "deposit.created" || row.MsgID != event.MsgID || row.Attempts != 0 ||
		row.ClaimedUntil != nil || row.LastError != nil || row.PublishedAt != nil {
		t.Errorf("row = %+v", row)
	}
	var stored any
	if err := json.Unmarshal([]byte(row.Payload), &stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, event.Payload) {
		t.Errorf("payload = %s", row.Payload)
	}
}

func TestEnqueueRollsBackWithItsTransaction(t *testing.T) {
	pool, created := setUp(t)
	event := testEvent(created)
	failed := errors.New("the state change failed")
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		if err := outbox.Enqueue(context.Background(), tx, event); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("err = %v", err)
	}
	if rows := relaytest.Rows(t, pool, depositOf(event)); len(rows) != 0 {
		t.Errorf("rows = %v", rows)
	}
}

func TestEnqueueFailsOnASecondEventWithTheSameMsgID(t *testing.T) {
	pool, created := setUp(t)
	first := testEvent(created)
	enqueueAll(t, pool, first)
	second := testEvent(created)
	second.Entity, second.MsgID = first.Entity, first.MsgID
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		return outbox.Enqueue(context.Background(), tx, second)
	})
	if err == nil || !strings.Contains(err.Error(), "already holds an event with msg_id") {
		t.Errorf("err = %v", err)
	}
	rows := relaytest.Rows(t, pool, depositOf(first))
	if len(rows) != 1 || rows[0].ID != first.ID.UUID().String() {
		t.Errorf("rows = %v", rows)
	}
}

func TestClaimsInIDOrderLeasingEachEventAndCountingTheAttempt(t *testing.T) {
	pool, created := setUp(t)
	events := []outbox.NewEvent{testEvent(created), testEvent(created), testEvent(created)}
	enqueueAll(t, pool, events...)

	if got := claimOnly(t, pool, 30*time.Second, events...); !slices.Equal(got, idOrder(events...)) {
		t.Errorf("claimed %v, want %v", got, idOrder(events...))
	}
	for _, e := range events {
		row := relaytest.OneRow(t, pool, depositOf(e))
		if row.Attempts != 1 || row.ClaimedUntil == nil || time.Until(*row.ClaimedUntil) < 25*time.Second {
			t.Errorf("row = %+v", row)
		}
	}
}

func TestClaimRespectsTheLimitOldestFirst(t *testing.T) {
	pool, created := setUp(t)
	events := []outbox.NewEvent{testEvent(created), testEvent(created), testEvent(created)}
	enqueueAll(t, pool, events...)

	claimed, err := outbox.Claim(context.Background(), pool, 2, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d", len(claimed))
	}
	// After the drain, these are the oldest claimable events (unless an earlier test's backed-off
	// event has just become claimable again, which would come first).
	newest := idOrder(events...)[2]
	for _, c := range claimed {
		if c.ID == newest {
			t.Error("claimed the newest event")
		}
	}
}

func TestClaimSkipsLeasedAndPublishedEventsAndReclaimsOnceTheLeaseExpires(t *testing.T) {
	pool, created := setUp(t)
	leased, published := testEvent(created), testEvent(created)
	enqueueAll(t, pool, leased, published)
	ctx := context.Background()

	got := claimOnly(t, pool, 300*time.Millisecond, leased, published)
	if !slices.Equal(got, idOrder(leased, published)) {
		t.Fatalf("claimed %v", got)
	}
	if r, err := outbox.MarkPublished(ctx, pool, published.ID); err != nil || r != outbox.Applied {
		t.Fatalf("MarkPublished: %v %v", r, err)
	}
	// Both leased: nothing of ours to claim yet.
	if got := claimOnly(t, pool, 30*time.Second, leased, published); len(got) != 0 {
		t.Errorf("claimed %v while leased", got)
	}

	time.Sleep(400 * time.Millisecond)
	// The lease has expired: claimed again, the second attempt. The published one never is.
	got = claimOnly(t, pool, 30*time.Second, leased, published)
	if !slices.Equal(got, []ids.EventID{leased.ID}) {
		t.Errorf("claimed %v after the lease", got)
	}
	if row := relaytest.OneRow(t, pool, depositOf(leased)); row.Attempts != 2 {
		t.Errorf("attempts = %d", row.Attempts)
	}
}

func TestTwoConcurrentClaimsGetDisjointEvents(t *testing.T) {
	pool, created := setUp(t)
	events := make([]outbox.NewEvent, 20)
	for i := range events {
		events[i] = testEvent(created)
	}
	enqueueAll(t, pool, events...)

	// Two claims at once, on two connections, each for at most 10. Neither waits for the other's
	// locks: each skips the rows the other has locked and takes the next ones. No event is claimed
	// twice, and between them they take all twenty.
	var a, b []outbox.Event
	var errA, errB error
	var wg sync.WaitGroup
	wg.Go(func() { a, errA = outbox.Claim(context.Background(), pool, 10, 30*time.Second) })
	wg.Go(func() { b, errB = outbox.Claim(context.Background(), pool, 10, 30*time.Second) })
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	seen := map[ids.EventID]int{}
	for _, e := range append(a, b...) {
		seen[e.ID]++
	}
	var ours []ids.EventID
	for id, n := range seen {
		if n > 1 {
			t.Errorf("%v claimed %d times", id, n)
		}
		if slices.Contains(*created, id) {
			ours = append(ours, id)
		}
	}
	slices.SortFunc(ours, byID)
	if !slices.Equal(ours, idOrder(events...)) {
		t.Errorf("between them they claimed %d of our 20", len(ours))
	}
}

// Go-specific: the claimed payload is compact JSON in jsonb's key order (shorter keys first), the
// bytes Node publishes after JSON.parse and JSON.stringify (see outbox.Claim).
func TestClaimGivesThePayloadAsCompactJSON(t *testing.T) {
	pool, created := setUp(t)
	event := testEvent(created)
	enqueueAll(t, pool, event)
	if row := relaytest.OneRow(t, pool, depositOf(event)); !strings.Contains(row.Payload, `": `) {
		t.Fatalf("expected jsonb's spaced text form in the column read as text: %s", row.Payload)
	}
	claimed, err := outbox.Claim(context.Background(), pool, 100, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"nested":{"amount_minor":"1250"},"event_id":"` + event.ID.TypeID() + `"}`
	for _, c := range claimed {
		if c.ID == event.ID {
			if string(c.Payload) != want {
				t.Errorf("payload %s\nwant    %s", c.Payload, want)
			}
			return
		}
	}
	t.Error("event not claimed")
}

func TestRecordsAFailureWithARetryDelayThenPublicationEachOnce(t *testing.T) {
	pool, created := setUp(t)
	event := testEvent(created)
	enqueueAll(t, pool, event)
	ctx := context.Background()
	if got := claimOnly(t, pool, 30*time.Second, event); len(got) != 1 {
		t.Fatalf("claimed %v", got)
	}

	r, err := outbox.MarkFailed(ctx, pool, event.ID, "stream unavailable", time.Minute)
	if err != nil || r != outbox.Applied {
		t.Fatalf("MarkFailed: %v %v", r, err)
	}
	row := relaytest.OneRow(t, pool, depositOf(event))
	if row.LastError == nil || *row.LastError != "stream unavailable" ||
		time.Until(*row.ClaimedUntil) < 55*time.Second {
		t.Errorf("after MarkFailed: %+v", row)
	}
	// Backed off: not claimable until then.
	if got := claimOnly(t, pool, 30*time.Second, event); len(got) != 0 {
		t.Errorf("claimed while backed off: %v", got)
	}

	if r, err := outbox.MarkPublished(ctx, pool, event.ID); err != nil || r != outbox.Applied {
		t.Fatalf("MarkPublished: %v %v", r, err)
	}
	row = relaytest.OneRow(t, pool, depositOf(event))
	if row.PublishedAt == nil || row.ClaimedUntil != nil || row.LastError != nil {
		t.Errorf("after MarkPublished: %+v", row)
	}

	// Already published: both are no-ops.
	if r, err := outbox.MarkPublished(ctx, pool, event.ID); err != nil || r != outbox.Skipped {
		t.Errorf("MarkPublished again: %v %v", r, err)
	}
	r, err = outbox.MarkFailed(ctx, pool, event.ID, "late failure", time.Second)
	if err != nil || r != outbox.Skipped {
		t.Errorf("MarkFailed after publication: %v %v", r, err)
	}
}
