// Package relaytest is what the outbox and relay tests share: the test NATS, publishers that record
// or fail, draining the outbox, and reading outbox rows. Node: the relay half of test/helpers.ts.
// Imported only by tests, like testdb (and the standard library's httptest).
package relaytest

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/contracttest"
	"github.com/guywillett/ledger-demo/go/internal/relay"
)

// The test NATS (compose nats-test, its streams created by nats-setup-test), shared with Node's
// tests (decision 52): the two suites aren't run at the same time.
const defaultNATSURL = "nats://localhost:4223"

// Conn connects to TEST_NATS_URL (default: the compose nats-test) and closes the connection when
// the test ends.
func Conn(t testing.TB) *broker.Conn {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		url = defaultNATSURL
	}
	conn, err := broker.Connect(url, "ledger-demo-go-test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("test NATS unreachable (is `docker compose up -d` running?): %v", err)
	}
	t.Cleanup(func() { conn.Close(broker.DrainTimeout) })
	return conn
}

// StreamPublisher is the relay's real publisher, on the test streams.
func StreamPublisher(t testing.TB, conn *broker.Conn) *broker.Publisher {
	t.Helper()
	p, err := broker.NewPublisher(conn, broker.PublishTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Published is one event a Recording publisher published.
type Published struct {
	Subject string
	Payload []byte
	MsgID   string
	Result  broker.PublishResult
}

// Recording wraps a publisher, checks every event against its contract, and records what was
// published, so tests can assert exactly which events the relay published.
type Recording struct {
	t    testing.TB
	next relay.Publisher
	mu   sync.Mutex // Publish may be called from the relay's goroutine while the test reads
	all  []Published
}

// NewRecording records what next publishes.
func NewRecording(t testing.TB, next relay.Publisher) *Recording {
	return &Recording{t: t, next: next}
}

// Publish checks the event against its contract, publishes it through the wrapped publisher, and
// records it.
func (r *Recording) Publish(
	ctx context.Context,
	subject string,
	payload []byte,
	msgID string,
) (broker.PublishResult, error) {
	contracttest.CheckEvent(r.t, payload)
	result, err := r.next.Publish(ctx, subject, payload, msgID)
	if err != nil {
		return result, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.all = append(r.all,
		Published{Subject: subject, Payload: payload, MsgID: msgID, Result: result})
	return result, nil
}

// WithMsgID is what was published with this message ID, in order.
func (r *Recording) WithMsgID(msgID string) []Published {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []Published
	for _, p := range r.all {
		if p.MsgID == msgID {
			found = append(found, p)
		}
	}
	return found
}

// MsgIDs is every published message ID, in order.
func (r *Recording) MsgIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, len(r.all))
	for i, p := range r.all {
		ids[i] = p.MsgID
	}
	return ids
}

// Failing fails every publish, as Node's failingPublisher.
type Failing struct{}

// Publish fails.
func (Failing) Publish(context.Context, string, []byte, string) (broker.PublishResult, error) {
	return broker.PublishResult{}, errors.New("simulated publish failure")
}

// Deps is the relay's dependencies for a test, as Node's testRelayDeps: batches of 100 under a
// 30 s lease, a 1 s retry delay, a logger that discards (pass one to capture lines).
func Deps(pool *pgxpool.Pool, publisher relay.Publisher, logger *slog.Logger) relay.Deps {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return relay.Deps{
		Pool:       pool,
		Publisher:  publisher,
		Logger:     logger,
		BatchSize:  100,
		Lease:      30 * time.Second,
		RetryDelay: func(int) time.Duration { return time.Second },
	}
}

// Drain publishes every claimable event, so a test's own events are the only ones left to claim
// (except, rarely, an earlier test's backed-off event becoming claimable again, so assertions look
// only at the test's own events).
func Drain(t testing.TB, pool *pgxpool.Pool, publisher relay.Publisher) {
	t.Helper()
	d := Deps(pool, publisher, nil)
	for {
		batch, err := relay.PublishBatch(context.Background(), d)
		if err != nil {
			t.Fatal(err)
		}
		if batch.Claimed == 0 {
			return
		}
	}
}

// Row is an outbox row as the tests look at it.
type Row struct {
	ID           string
	EntityType   string
	EntityID     string
	Subject      string
	MsgID        string
	Payload      string // jsonb's text form
	Attempts     int
	ClaimedUntil *time.Time
	LastError    *string
	PublishedAt  *time.Time
}

// Rows reads the outbox rows about an entity (a UUID), oldest first.
func Rows(t testing.TB, pool *pgxpool.Pool, entityID any) []Row {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT id::text, entity_type, entity_id::text, subject, msg_id, payload, attempts, claimed_until,
       last_error, published_at
FROM outbox WHERE entity_id = $1 ORDER BY id`, entityID)
	if err != nil {
		t.Fatal(err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Row, error) {
		var r Row
		err := row.Scan(&r.ID, &r.EntityType, &r.EntityID, &r.Subject, &r.MsgID, &r.Payload,
			&r.Attempts, &r.ClaimedUntil, &r.LastError, &r.PublishedAt)
		return r, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// OneRow is the single outbox row about an entity.
func OneRow(t testing.TB, pool *pgxpool.Pool, entityID any) Row {
	t.Helper()
	rows := Rows(t, pool, entityID)
	if len(rows) != 1 {
		t.Fatalf("%d outbox rows for %v, want 1", len(rows), entityID)
	}
	return rows[0]
}
