package consumer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/logging"
)

// fakeMessage is a Message that records its signals, as Node's tests' fake: no NATS, and a
// "redelivery" is delivering again. onSignal, if set, runs at each signal (before it is recorded),
// so a test can look at the delivery log at that moment.
type fakeMessage struct {
	body     string
	msgID    string
	attempt  int
	failSend error // returned by every signal, as a closed connection would

	mu       sync.Mutex
	calls    []string
	nakDelay time.Duration
	reason   string
	onSignal func(signal string)
}

func message(body string) *fakeMessage {
	return &fakeMessage{body: body, msgID: "msg-" + ids.New[ids.Event]().TypeID(), attempt: 1}
}

func (m *fakeMessage) Body() []byte         { return []byte(m.body) }
func (m *fakeMessage) NATSMsgID() string    { return m.msgID }
func (m *fakeMessage) DeliveryAttempt() int { return m.attempt }

func (m *fakeMessage) signal(name string) error {
	if m.onSignal != nil {
		m.onSignal(name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, name)
	return m.failSend
}

func (m *fakeMessage) Ack() error { return m.signal("ack") }
func (m *fakeMessage) Term(reason string) error {
	m.reason = reason
	return m.signal("term")
}
func (m *fakeMessage) Nak(delay time.Duration) error {
	m.nakDelay = delay
	return m.signal("nak")
}
func (m *fakeMessage) Working() error { return m.signal("working") }

func (m *fakeMessage) signals() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// testEvent is the lifecycle tests' event: an event_id and a number.
type testEvent struct {
	EventID ids.EventID
	N       int
}

// decodeTestEvent requires a JSON object with an evt TypeID event_id and a number n.
func decodeTestEvent(body []byte) (testEvent, error) {
	var wire struct {
		EventID *ids.EventID `json:"event_id"`
		N       *int         `json:"n"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return testEvent{}, err
	}
	if wire.EventID == nil || wire.N == nil {
		return testEvent{}, errors.New("event_id and n are required")
	}
	return testEvent{EventID: *wire.EventID, N: *wire.N}, nil
}

// eventBody is a valid test event, and its event_id.
func eventBody(n int) (string, ids.EventID) {
	id := ids.New[ids.Event]()
	data, _ := json.Marshal(map[string]any{"event_id": id.TypeID(), "n": n})
	return string(data), id
}

// capture is a logger whose lines the test can read back.
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func newCapture(t *testing.T) (*capture, *slog.Logger) {
	t.Helper()
	c := &capture{}
	logger, err := logging.New(c, "test", "debug")
	if err != nil {
		t.Fatal(err)
	}
	return c, logger
}

// lines are the log lines, each decoded; with msg, only those with that message.
func (c *capture) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && (msg == "" || m["msg"] == msg) {
			found = append(found, m)
		}
	}
	return found
}

// one is the single line with this message.
func (c *capture) one(t *testing.T, msg string) map[string]any {
	t.Helper()
	lines := c.lines(t, msg)
	if len(lines) != 1 {
		t.Fatalf("%d lines %q, want 1; all lines: %v", len(lines), msg, c.lines(t, ""))
	}
	return lines[0]
}

// delivery is an event_deliveries row as the tests look at it.
type delivery struct {
	Outcome         string
	EventID         *string
	NATSMsgID       *string
	DeliveryAttempt int
	Consumer        string
	Completed       bool
}

// deliveriesByMsgID reads the delivery rows for a message ID, oldest first.
func deliveriesByMsgID(t *testing.T, pool *pgxpool.Pool, msgID string) []delivery {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT outcome, event_id::text, nats_msg_id, delivery_attempt, consumer, completed_at IS NOT NULL
FROM event_deliveries WHERE nats_msg_id = $1 ORDER BY id`, msgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []delivery
	for rows.Next() {
		var d delivery
		err := rows.Scan(&d.Outcome, &d.EventID, &d.NATSMsgID, &d.DeliveryAttempt, &d.Consumer,
			&d.Completed)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

// outcomeNow is the outcome of the message's (latest) delivery row right now.
func outcomeNow(t *testing.T, pool *pgxpool.Pool, msgID string) string {
	t.Helper()
	rows := deliveriesByMsgID(t, pool, msgID)
	if len(rows) == 0 {
		return ""
	}
	return rows[len(rows)-1].Outcome
}

// failingFinish is the pool with every delivery-log UPDATE failing (Node's failingDeliveryFinish):
// the delivery is started, its outcome can't be recorded. Embedding the interface gives the struct
// all of db.Querier's methods from the pool; Exec is overridden.
type failingFinish struct {
	db.Querier
}

func (f failingFinish) Exec(
	ctx context.Context,
	sql string,
	args ...any,
) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE event_deliveries") {
		return pgconn.CommandTag{}, errors.New("simulated delivery log failure")
	}
	return f.Querier.Exec(ctx, sql, args...)
}

// lifecycle is the tests' lifecycle on the given pool and logger: a 1 s retry, doubling to 8 s, and
// errBug as the only bug.
func lifecycle(pool db.Querier, logger *slog.Logger) consumer.Lifecycle[testEvent] {
	return consumer.Lifecycle[testEvent]{
		Consumer:  consumer.DepositWorker,
		EventType: "test.event",
		Decode:    decodeTestEvent,
		Pool:      pool,
		Logger:    logger,
		Retry:     backoffForTests,
		IsBug:     func(err error) bool { return errors.Is(err, errBug) },
	}
}

var errBug = errors.New("simulated invariant violation")
