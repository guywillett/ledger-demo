package relay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ingestion"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/outbox"
	"github.com/guywillett/ledger-demo/go/internal/relay"
	"github.com/guywillett/ledger-demo/go/internal/relaytest"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// PublishBatch and Poll against the relay test database and the test NATS streams: the cases of
// node/test/relay/relay.test.ts. This package holds the database's lock while it runs (no outbox
// test runs meanwhile), and each test drains the outbox first, so its own events are the only ones
// left to claim (except, rarely, an earlier test's backed-off event becoming claimable, so
// assertions look only at the test's own events).
func TestMain(m *testing.M) {
	os.Exit(testdb.Exclusive(m, testdb.Relay))
}

type env struct {
	pool   *pgxpool.Pool
	conn   *broker.Conn
	stream *broker.Publisher
}

func setUp(t *testing.T) env {
	t.Helper()
	e := env{pool: testdb.PoolFor(t, testdb.Relay), conn: relaytest.Conn(t)}
	e.stream = relaytest.StreamPublisher(t, e.conn)
	relaytest.Drain(t, e.pool, e.stream)
	return e
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
	logger, err := logging.New(c, "relay", "info")
	if err != nil {
		t.Fatal(err)
	}
	return c, logger
}

// lines are the log lines with this message.
func (c *capture) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil && m["msg"] == msg {
			found = append(found, m)
		}
	}
	return found
}

// newDeposit records a notification for a fresh account, as the webhook does: a deposit in status
// created and its deposit.created event in the outbox.
func newDeposit(
	t *testing.T,
	pool *pgxpool.Pool,
	amount money.Amount,
) (ids.DepositID, testdb.Account) {
	t.Helper()
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	var result ingestion.Result
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		var err error
		result, err = ingestion.RecordNotification(ctx, tx, ingestion.Notification{
			CreditorIBAN:         sepa.IBAN(account.IBAN),
			Amount:               amount,
			Currency:             "EUR",
			Network:              "SEPA",
			NetworkTransactionID: "go-relay-test-" + ids.New[ids.Event]().TypeID(),
		})
		return err
	})
	if err != nil || result.Kind != ingestion.Created {
		t.Fatalf("recording a deposit: %v %v", result.Kind, err)
	}
	return result.Deposit, account
}

func msgIDOf(deposit ids.DepositID) string { return events.DepositCreatedMessageID(deposit) }

// storedMessage reads a message back from a stream: what consumers will actually get.
func storedMessage(
	t *testing.T,
	conn *broker.Conn,
	stream string,
	seq uint64,
) *jetstream.RawStreamMsg {
	t.Helper()
	js, err := jetstream.New(conn.NATS())
	if err != nil {
		t.Fatal(err)
	}
	s, err := js.Stream(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := s.GetMsg(context.Background(), seq)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestPublishesEachEventWithItsMsgIDAndMarksItPublished(t *testing.T) {
	e := setUp(t)
	first, _ := newDeposit(t, e.pool, 1250)
	second, _ := newDeposit(t, e.pool, 1250)
	deps := []ids.DepositID{first, second}
	rec := relaytest.NewRecording(t, e.stream)
	logs, logger := newCapture(t)

	batch, err := relay.PublishBatch(context.Background(), relaytest.Deps(e.pool, rec, logger))
	if err != nil {
		t.Fatal(err)
	}
	if batch.Claimed < 2 || batch.Published != batch.Claimed {
		t.Errorf("batch = %+v", batch)
	}

	for _, deposit := range deps {
		row := relaytest.OneRow(t, e.pool, deposit)
		if row.Attempts != 1 || row.LastError != nil || row.ClaimedUntil != nil || row.PublishedAt == nil {
			t.Errorf("row = %+v", row)
		}
		ours := rec.WithMsgID(msgIDOf(deposit))
		if len(ours) != 1 || ours[0].Subject != "deposit.created" || ours[0].Result.Duplicate {
			t.Fatalf("published = %+v", ours)
		}
		stored := storedMessage(t, e.conn, broker.StreamDeposits, ours[0].Result.Seq)
		if stored.Header.Get("Nats-Msg-Id") != msgIDOf(deposit) {
			t.Errorf("Nats-Msg-Id = %q", stored.Header.Get("Nats-Msg-Id"))
		}
		// The bytes Node publishes: the row's payload, compact.
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(row.Payload)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored.Data, compact.Bytes()) {
			t.Errorf("stored %s\nwant   %s", stored.Data, compact.Bytes())
		}
		var decoded map[string]any
		if err := json.Unmarshal(stored.Data, &decoded); err != nil {
			t.Fatal(err)
		}
		// The outbox row's ID is the event_id.
		if id := relaytestEventID(t, row.ID); decoded["event_id"] != id {
			t.Errorf("event_id = %v, want %v", decoded["event_id"], id)
		}
	}

	// In id order: the earlier deposit's event first.
	var order []string
	for _, id := range rec.MsgIDs() {
		if id == msgIDOf(first) || id == msgIDOf(second) {
			order = append(order, id)
		}
	}
	if !slices.Equal(order, []string{msgIDOf(first), msgIDOf(second)}) {
		t.Errorf("order = %v", order)
	}

	logged := logs.lines(t, "event published")
	if len(logged) != batch.Claimed {
		t.Errorf("%d event published lines for %d claimed", len(logged), batch.Claimed)
	}
	var line map[string]any
	for _, l := range logged {
		if l["deposit_id"] == first.TypeID() {
			line = l
		}
	}
	if line == nil || line["level"] != "info" || !strings.HasPrefix(line["event_id"].(string), "evt_") ||
		line["subject"] != "deposit.created" || line["stream_seq"] == nil {
		t.Errorf("line = %v", line)
	}
	if _, ok := line["stream_duplicate"]; ok {
		t.Errorf("stream_duplicate on a first publish: %v", line)
	}
	if _, ok := line["already_marked"]; ok {
		t.Errorf("already_marked: %v", line)
	}
}

// relaytestEventID turns an outbox row's UUID into its event TypeID.
func relaytestEventID(t *testing.T, uuid string) string {
	t.Helper()
	var id ids.EventID
	if err := id.Scan(uuid); err != nil {
		t.Fatal(err)
	}
	return id.TypeID()
}

func TestRecordsAFailureAndPublishesAfterTheRetryDelay(t *testing.T) {
	e := setUp(t)
	deposit, _ := newDeposit(t, e.pool, 1250)
	logs, logger := newCapture(t)
	failing := relaytest.Deps(e.pool, relaytest.Failing{}, logger)
	failing.RetryDelay = func(int) time.Duration { return 300 * time.Millisecond }

	batch, err := relay.PublishBatch(context.Background(), failing)
	if err != nil || batch.Published != 0 {
		t.Fatalf("batch = %+v, %v", batch, err)
	}
	row := relaytest.OneRow(t, e.pool, deposit)
	if row.Attempts != 1 || row.LastError == nil || *row.LastError != "simulated publish failure" ||
		row.PublishedAt != nil {
		t.Errorf("row = %+v", row)
	}
	var failure map[string]any
	for _, l := range logs.lines(t, "publish failed; will retry") {
		if l["deposit_id"] == deposit.TypeID() {
			failure = l
		}
	}
	if failure == nil || failure["level"] != "warn" || failure["attempts"] != 1.0 ||
		failure["retry_in_ms"] != 300.0 || failure["subject"] != "deposit.created" ||
		!strings.HasPrefix(failure["event_id"].(string), "evt_") {
		t.Errorf("failure line = %v", failure)
	}

	// Backed off: a batch straight away doesn't touch it.
	rec := relaytest.NewRecording(t, e.stream)
	if _, err := relay.PublishBatch(context.Background(), relaytest.Deps(e.pool, rec, nil)); err != nil {
		t.Fatal(err)
	}
	if got := rec.WithMsgID(msgIDOf(deposit)); len(got) != 0 {
		t.Errorf("published while backed off: %v", got)
	}

	// After the delay, the next batch publishes it: the second attempt.
	time.Sleep(400 * time.Millisecond)
	if _, err := relay.PublishBatch(context.Background(), relaytest.Deps(e.pool, rec, nil)); err != nil {
		t.Fatal(err)
	}
	if got := rec.WithMsgID(msgIDOf(deposit)); len(got) != 1 {
		t.Errorf("published %d times after the delay", len(got))
	}
	row = relaytest.OneRow(t, e.pool, deposit)
	if row.Attempts != 2 || row.LastError != nil || row.PublishedAt == nil {
		t.Errorf("row = %+v", row)
	}
}

func TestPublishesAgainAfterARelayCrashedBetweenPublishingAndMarkingAndTheStreamDropsTheRepeat(t *testing.T) {
	e := setUp(t)
	ctx := context.Background()
	deposit, _ := newDeposit(t, e.pool, 1250)

	// The "crashed" relay: claims with a short lease and publishes, but never marks.
	claimed, err := outbox.Claim(ctx, e.pool, 100, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(claimed, func(ev outbox.Event) bool { return ev.MsgID == msgIDOf(deposit) })
	if i < 0 {
		t.Fatal("the deposit's event was not claimed")
	}
	event := claimed[i]
	first, err := e.stream.Publish(ctx, event.Subject, event.Payload, event.MsgID)
	if err != nil || first.Duplicate {
		t.Fatalf("first publish: %+v %v", first, err)
	}

	// Before the lease expires, another relay leaves it alone.
	rec := relaytest.NewRecording(t, e.stream)
	if _, err := relay.PublishBatch(ctx, relaytest.Deps(e.pool, rec, nil)); err != nil {
		t.Fatal(err)
	}
	if got := rec.WithMsgID(event.MsgID); len(got) != 0 {
		t.Errorf("published while leased: %v", got)
	}

	// Once it has expired, the event is claimed and published again, with the same msg_id: the
	// stream recognises the repeat within its duplicate window and stores nothing new.
	time.Sleep(400 * time.Millisecond)
	logs, logger := newCapture(t)
	if _, err := relay.PublishBatch(ctx, relaytest.Deps(e.pool, rec, logger)); err != nil {
		t.Fatal(err)
	}
	again := rec.WithMsgID(event.MsgID)
	if len(again) != 1 || !again[0].Result.Duplicate || again[0].Result.Seq != first.Seq {
		t.Fatalf("again = %+v, first seq %d", again, first.Seq)
	}
	// The same event, with the same event_id: it is the outbox row, not rebuilt.
	if !bytes.Equal(again[0].Payload, event.Payload) {
		t.Errorf("payload %s, want %s", again[0].Payload, event.Payload)
	}
	row := relaytest.OneRow(t, e.pool, deposit)
	if row.Attempts != 2 || row.LastError != nil || row.PublishedAt == nil {
		t.Errorf("row = %+v", row)
	}
	var line map[string]any
	for _, l := range logs.lines(t, "event published") {
		if l["event_id"] == event.ID.TypeID() {
			line = l
		}
	}
	if line == nil || line["stream_duplicate"] != true {
		t.Errorf("line = %v", line)
	}
}

// fund puts amount in the account's available balance the real way: its deposit booked, reviewed,
// approved and credited (Node's fundTestAccount).
func fund(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID) {
	t.Helper()
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		ctx := context.Background()
		if _, err := ledger.Book(ctx, tx, deposit); err != nil {
			return err
		}
		for _, tr := range []deposits.Transition{deposits.StartReview, deposits.Approve} {
			if _, _, err := deposits.TransitionStatus(ctx, tx, deposit, tr, ""); err != nil {
				return err
			}
		}
		_, err := ledger.Credit(ctx, tx, deposit)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// newHeldWithdrawal creates a withdrawal and stores its withdrawal.held event in the same
// transaction, as the withdrawal request will (step 7; Node's createTestWithdrawal).
func newHeldWithdrawal(
	t *testing.T,
	pool *pgxpool.Pool,
	account testdb.Account,
	amount money.Amount,
) withdrawals.Withdrawal {
	t.Helper()
	ctx := context.Background()
	var w withdrawals.Withdrawal
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		created, ok, err := ledger.CreateWithdrawal(ctx, tx, withdrawals.NewWithdrawal{
			Account:        account.ID,
			IdempotencyKey: ids.New[ids.Event]().TypeID(),
			Amount:         amount,
			CreditorIBAN:   "DE75512108001245126199",
			CreditorName:   "Jane Doe",
		})
		if err != nil || !ok {
			t.Fatalf("CreateWithdrawal: %v %v", ok, err)
		}
		w = created
		id := ids.New[ids.Event]()
		return outbox.Enqueue(ctx, tx, outbox.NewEvent{
			ID:      id,
			Entity:  outbox.Withdrawal(w.ID),
			Subject: events.SubjectWithdrawalHeld,
			MsgID:   events.WithdrawalMessageID(w.ID, "held"),
			Payload: events.WithdrawalHeldV1{
				EventID:       id,
				EventType:     events.SubjectWithdrawalHeld,
				SchemaVersion: 1,
				WithdrawalID:  w.ID,
				AccountID:     w.Account,
				AmountMinor:   w.Amount,
				Currency:      w.Currency,
				OccurredAt:    timestamp.Time(w.CreatedAt),
			},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPublishesEachEventToItsOwnStream(t *testing.T) {
	e := setUp(t)
	deposit, account := newDeposit(t, e.pool, 1000)
	fund(t, e.pool, deposit)
	withdrawal := newHeldWithdrawal(t, e.pool, account, 400)
	rec := relaytest.NewRecording(t, e.stream)
	logs, logger := newCapture(t)

	if _, err := relay.PublishBatch(context.Background(), relaytest.Deps(e.pool, rec, logger)); err != nil {
		t.Fatal(err)
	}

	depositEvent := rec.WithMsgID(msgIDOf(deposit))
	withdrawalEvent := rec.WithMsgID(withdrawal.ID.TypeID() + ".held")
	if len(depositEvent) != 1 || depositEvent[0].Result.Stream != broker.StreamDeposits {
		t.Errorf("deposit event = %+v", depositEvent)
	}
	if len(withdrawalEvent) != 1 || withdrawalEvent[0].Result.Stream != broker.StreamWithdrawals ||
		withdrawalEvent[0].Subject != "withdrawal.held" {
		t.Fatalf("withdrawal event = %+v", withdrawalEvent)
	}
	stored := storedMessage(t, e.conn, broker.StreamWithdrawals, withdrawalEvent[0].Result.Seq)
	if !bytes.Equal(stored.Data, withdrawalEvent[0].Payload) {
		t.Errorf("stored %s, published %s", stored.Data, withdrawalEvent[0].Payload)
	}
	if row := relaytest.OneRow(t, e.pool, withdrawal.ID); row.PublishedAt == nil {
		t.Errorf("row = %+v", row)
	}

	// The relay's log line names the withdrawal.
	var line map[string]any
	for _, l := range logs.lines(t, "event published") {
		if l["withdrawal_id"] == withdrawal.ID.TypeID() {
			line = l
		}
	}
	if line == nil || line["subject"] != "withdrawal.held" {
		t.Errorf("line = %v", line)
	}
	if _, ok := line["deposit_id"]; ok {
		t.Errorf("a withdrawal's line with a deposit_id: %v", line)
	}
}

func TestRecordsAnEventWithAnUnknownSubjectAsAPublishFailure(t *testing.T) {
	e := setUp(t)
	ctx := context.Background()
	id := ids.New[ids.Event]()
	deposit := ids.New[ids.Deposit]()
	err := db.InTx(ctx, e.pool, func(tx db.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.NewEvent{
			ID:      id,
			Entity:  outbox.Deposit(deposit),
			Subject: "refund.created",
			MsgID:   deposit.TypeID() + ".refunded",
			Payload: map[string]string{"event_id": id.TypeID()},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Not a real event: take it out of later drains.
	t.Cleanup(func() {
		_, err := e.pool.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = $1`, id)
		if err != nil {
			t.Error(err)
		}
	})

	if _, err := relay.PublishBatch(ctx, relaytest.Deps(e.pool, e.stream, nil)); err != nil {
		t.Fatal(err)
	}
	row := relaytest.OneRow(t, e.pool, deposit)
	if row.Attempts != 1 || row.LastError == nil || *row.LastError != "no stream for subject refund.created" ||
		row.PublishedAt != nil {
		t.Errorf("row = %+v", row)
	}
}

// nats/streams/<STREAM>.json, found from this file's location as contracttest finds the contract.
func streamDefinition(t *testing.T, stream string) map[string]any {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "../../../nats/streams", stream+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var definition map[string]any
	if err := json.Unmarshal(data, &definition); err != nil {
		t.Fatal(err)
	}
	return definition
}

func TestStreamsMatchTheirDefinitions(t *testing.T) {
	e := setUp(t)
	js, err := jetstream.New(e.conn.NATS())
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range broker.Streams {
		s, err := js.Stream(context.Background(), stream)
		if err != nil {
			t.Fatal(err)
		}
		// The server's config, in its own JSON names (as nats/streams/*.json is written).
		data, err := json.Marshal(s.CachedInfo().Config)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		for key, want := range streamDefinition(t, stream) {
			if !reflect.DeepEqual(config[key], want) {
				t.Errorf("%s.%s = %v, want %v", stream, key, config[key], want)
			}
		}
	}
}

// blocking is a publisher that waits to be released before publishing, so a test can act while a
// batch is in progress.
type blocking struct {
	next     relay.Publisher
	started  chan struct{} // closed when the first publish starts
	release  chan struct{} // closed by the test to let publishes through
	startOne sync.Once
}

func (b *blocking) Publish(
	ctx context.Context,
	subject string,
	payload []byte,
	msgID string,
) (broker.PublishResult, error) {
	b.startOne.Do(func() { close(b.started) })
	<-b.release
	return b.next.Publish(ctx, subject, payload, msgID)
}

// Go-specific (Node checks this by hand in its shutdown): cancelling Poll's context mid-batch, as a
// signal does, stops the loop but not the batch in progress, whose events are still published and
// marked, so a restarted relay doesn't publish them again.
func TestShutdownFinishesTheBatchInProgress(t *testing.T) {
	e := setUp(t)
	deposit, _ := newDeposit(t, e.pool, 1250)
	pub := &blocking{next: e.stream, started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		relay.Poll(ctx, relaytest.Deps(e.pool, pub, nil))
		close(stopped)
	}()

	select {
	case <-pub.started:
	case <-time.After(10 * time.Second):
		cancel()
		close(pub.release)
		t.Fatal("Poll's batch never reached the publisher")
	}
	cancel() // the signal, while the batch is publishing
	close(pub.release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Poll didn't return after cancellation")
	}
	if row := relaytest.OneRow(t, e.pool, deposit); row.PublishedAt == nil || row.Attempts != 1 {
		t.Errorf("the batch in progress didn't finish: %+v", row)
	}
}
