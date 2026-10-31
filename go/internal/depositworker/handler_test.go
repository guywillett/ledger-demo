package depositworker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/compliance"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/consumertest"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// The deposit-worker's handler against the test database, with fake messages: every case of
// node/test/deposit-worker/handler.test.ts. No NATS: a message here is what JetStream would
// deliver, and "redelivery" is calling the handler again. In the package itself (not depositworker_test) so
// one case can make the delivery log's writes fail through the unexported Deps.deliveryLog.

func decideAlways(d compliance.Decision) func() compliance.Decision {
	return func() compliance.Decision { return d }
}

var (
	approve = decideAlways(compliance.Approved(""))
	reject  = decideAlways(compliance.Rejected("test rejection"))
)

func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Pool:            testdb.Pool(t),
		Logger:          slog.New(slog.DiscardHandler),
		Decide:          approve,
		WorkingInterval: 10 * time.Millisecond,
		Retry:           backoff.Backoff{Base: time.Second, Max: 60 * time.Second},
	}
}

type testDeposit struct {
	id      ids.DepositID
	account testdb.Account
	amount  money.Amount
}

func newDeposit(t *testing.T, pool *pgxpool.Pool) testDeposit {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	id := testdb.NewDeposit(t, pool, account, 1250)
	return testDeposit{id: id, account: account, amount: 1250}
}

// eventFor is the deposit's event exactly as ingestion stores it, as JSON, and its event_id.
func eventFor(d testDeposit) (string, ids.EventID) {
	id := ids.New[ids.Event]()
	body, err := json.Marshal(events.DepositCreatedV1{
		EventID:              id,
		EventType:            events.SubjectDepositCreated,
		SchemaVersion:        1,
		DepositID:            d.id,
		Network:              "SEPA",
		NetworkTransactionID: "go-test-" + d.id.TypeID(),
		AccountID:            d.account.ID,
		AmountMinor:          d.amount,
		Currency:             "EUR",
		OccurredAt:           timestamp.Time(time.Now().UTC().Truncate(time.Millisecond)),
	})
	if err != nil {
		panic(err)
	}
	return string(body), id
}

// settled fails the test unless the delivery ended with want, the message having received exactly
// these signals.
func settled(
	t *testing.T,
	got consumer.Outcome,
	m *consumertest.Message,
	want consumer.Outcome,
	calls ...string,
) {
	t.Helper()
	if got != want || !slices.Equal(m.Calls(), calls) {
		t.Fatalf("outcome %s, signals %v; want %s, %v", got, m.Calls(), want, calls)
	}
}

func message(body, msgID string, attempt int) *consumertest.Message {
	return consumertest.NewMessage(body, msgID, attempt)
}

func query[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) []T {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []T
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		found = append(found, v)
	}
	return found
}

func statusOf(t *testing.T, pool *pgxpool.Pool, id ids.DepositID) string {
	t.Helper()
	return query[string](t, pool, `SELECT status FROM deposits WHERE id = $1`, id)[0]
}

func historyOf(t *testing.T, pool *pgxpool.Pool, id ids.DepositID) []string {
	t.Helper()
	return query[string](t, pool,
		`SELECT to_status FROM deposit_status_history WHERE deposit_id = $1 ORDER BY id`, id)
}

func movementSteps(t *testing.T, pool *pgxpool.Pool, id ids.DepositID) []string {
	t.Helper()
	return query[string](t, pool, `
SELECT reference_step FROM balance_movements
WHERE reference_type = 'deposit' AND reference_id = $1 ORDER BY created_at, id`, id)
}

type balances struct{ pendingIn, available, held int64 }

func balancesOf(t *testing.T, pool *pgxpool.Pool, account ids.AccountID) balances {
	t.Helper()
	var b balances
	err := pool.QueryRow(context.Background(),
		`SELECT pending_in, available, held FROM account_balances WHERE account_id = $1`, account).
		Scan(&b.pendingIn, &b.available, &b.held)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type deliveryRow struct {
	outcome   string
	natsMsgID *string
	attempt   int
}

func deliveriesOf(t *testing.T, pool *pgxpool.Pool, eventID ids.EventID) []deliveryRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT outcome, nats_msg_id, delivery_attempt FROM event_deliveries
WHERE event_id = $1 ORDER BY id`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []deliveryRow
	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.outcome, &d.natsMsgID, &d.attempt); err != nil {
			t.Fatal(err)
		}
		found = append(found, d)
	}
	return found
}

func outcomes(rows []deliveryRow) []string {
	var o []string
	for _, r := range rows {
		o = append(o, r.outcome)
	}
	return o
}

func step[T any](
	t *testing.T,
	pool *pgxpool.Pool,
	f func(ctx context.Context, tx db.Tx) (T, error),
) {
	t.Helper()
	if _, err := inTx(context.Background(), pool, f); err != nil {
		t.Fatal(err)
	}
}

func book(id ids.DepositID) func(context.Context, db.Tx) (ledger.Result, error) {
	return func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
		return ledger.Book(ctx, tx, id)
	}
}

func startReview(id ids.DepositID) func(context.Context, db.Tx) (compliance.Result, error) {
	return func(ctx context.Context, tx db.Tx) (compliance.Result, error) {
		return compliance.StartReview(ctx, tx, id)
	}
}

func decide(
	id ids.DepositID,
	d compliance.Decision,
) func(context.Context, db.Tx) (compliance.Result, error) {
	return func(ctx context.Context, tx db.Tx) (compliance.Result, error) {
		return compliance.Decide(ctx, tx, id, d)
	}
}

func TestBooksReviewsDecidesAndCreditsAnApprovedDepositThenAcks(t *testing.T) {
	d := testDeps(t)
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	deposit := newDeposit(t, d.Pool)
	body, eventID := eventFor(deposit)
	msgID := deposit.id.TypeID() + ".created"
	m := message(body, msgID, 1)

	settled(t, Handle(t.Context(), d, m), m, consumer.Applied, "ack")
	if s := statusOf(t, d.Pool, deposit.id); s != "credited" {
		t.Errorf("status %s", s)
	}
	if b := balancesOf(t, d.Pool, deposit.account.ID); b != (balances{0, 1250, 0}) {
		t.Errorf("balances %+v", b)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book", "credit"}) {
		t.Errorf("movements %v", s)
	}
	rows := deliveriesOf(t, d.Pool, eventID)
	if len(rows) != 1 || rows[0].outcome != "applied" || *rows[0].natsMsgID != msgID ||
		rows[0].attempt != 1 {
		t.Errorf("deliveries %+v", rows)
	}
	line := logs.Line("deposit processed")
	if line["event_id"] != eventID.TypeID() || line["deposit_id"] != deposit.id.TypeID() ||
		line["booked"] != "applied" || line["review_started"] != "applied" ||
		line["decided"] != "approved" || line["posted"] != "credited" {
		t.Errorf("line = %v", line)
	}
}

func TestReturnsARejectedDepositToTheSender(t *testing.T) {
	d := testDeps(t)
	d.Decide = reject
	deposit := newDeposit(t, d.Pool)
	body, _ := eventFor(deposit)
	if got := Handle(t.Context(), d, message(body, "m", 1)); got != consumer.Applied {
		t.Fatalf("outcome %s", got)
	}
	if s := statusOf(t, d.Pool, deposit.id); s != "returned" {
		t.Errorf("status %s", s)
	}
	if b := balancesOf(t, d.Pool, deposit.account.ID); b != (balances{}) {
		t.Errorf("balances %+v", b)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book", "return"}) {
		t.Errorf("movements %v", s)
	}
}

func TestAppliesTheSameEventOnceWhenDeliveredTwiceWithDifferentMessageIDs(t *testing.T) {
	d := testDeps(t)
	deposit := newDeposit(t, d.Pool)
	body, eventID := eventFor(deposit)

	if got := Handle(t.Context(), d, message(body, "first", 1)); got != consumer.Applied {
		t.Fatalf("first: %s", got)
	}
	second := message(body, "second", 1)
	settled(t, Handle(t.Context(), d, second), second, consumer.Duplicate, "ack")
	rows := deliveriesOf(t, d.Pool, eventID)
	if len(rows) != 2 || rows[0].outcome != "applied" || *rows[0].natsMsgID != "first" ||
		rows[1].outcome != "duplicate" || *rows[1].natsMsgID != "second" {
		t.Errorf("deliveries %+v", rows)
	}
	if b := balancesOf(t, d.Pool, deposit.account.ID); b != (balances{0, 1250, 0}) {
		t.Errorf("balances %+v", b)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book", "credit"}) {
		t.Errorf("movements %v", s)
	}
}

// Two consumers handle the same event at once: two goroutines released together by a closed
// channel (the starting gun: every receiver wakes when it closes), under -race.
func TestAppliesTheSameEventOnceWhenTwoConsumersProcessItConcurrently(t *testing.T) {
	d := testDeps(t)
	d.ReviewDelay = 30 * time.Millisecond // so the two runs overlap
	deposit := newDeposit(t, d.Pool)
	body, _ := eventFor(deposit)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, msgID := range []string{"a", "b"} {
		wg.Go(func() {
			<-start
			Handle(context.Background(), d, message(body, msgID, 1))
		})
	}
	close(start)
	wg.Wait()

	// Each did whatever steps it got to first; the money moved once.
	if b := balancesOf(t, d.Pool, deposit.account.ID); b != (balances{0, 1250, 0}) {
		t.Errorf("balances %+v", b)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book", "credit"}) {
		t.Errorf("movements %v", s)
	}
	want := []string{"created", "received", "under_review", "approved", "credited"}
	if h := historyOf(t, d.Pool, deposit.id); !slices.Equal(h, want) {
		t.Errorf("history %v", h)
	}
}

// A crash after Tx N leaves exactly Tx 1..N committed plus a delivery row left in_progress (each
// Tx commits on its own). These tests create that state directly, with the same step functions
// the handler runs, then run the handler as the redelivery: the state is the seam, as in Node, so
// the handler needs no hook for crashing.
func TestCompletesAfterACrashBetweenBookingAndTheDecisionCreditingPendingInOnce(t *testing.T) {
	d := testDeps(t)
	deposit := newDeposit(t, d.Pool)
	body, eventID := eventFor(deposit)
	_, err := consumer.StartDelivery(t.Context(), d.Pool, consumer.NewDelivery{
		Consumer: consumer.DepositWorker, EventID: &eventID, NATSMsgID: "crashed",
		DeliveryAttempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	step(t, d.Pool, book(deposit.id)) // Tx 1, then the crash

	if got := Handle(t.Context(), d, message(body, "crashed", 2)); got != consumer.Applied {
		t.Fatalf("outcome %s", got)
	}
	if b := balancesOf(t, d.Pool, deposit.account.ID); b != (balances{0, 1250, 0}) {
		t.Errorf("balances %+v", b)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book", "credit"}) {
		t.Errorf("movements %v", s)
	}
	rows := deliveriesOf(t, d.Pool, eventID)
	if len(rows) != 2 || rows[0].outcome != "in_progress" || rows[0].attempt != 1 ||
		rows[1].outcome != "applied" || rows[1].attempt != 2 {
		t.Errorf("deliveries %+v", rows)
	}
}

func TestPostsTheRecordedDecisionAfterACrashBetweenDecidingAndPostingNeverReDeciding(t *testing.T) {
	d := testDeps(t)
	deposit := newDeposit(t, d.Pool)
	body, eventID := eventFor(deposit)
	_, err := consumer.StartDelivery(t.Context(), d.Pool, consumer.NewDelivery{
		Consumer: consumer.DepositWorker, EventID: &eventID, NATSMsgID: "crashed",
		DeliveryAttempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	step(t, d.Pool, book(deposit.id))                            // Tx 1
	step(t, d.Pool, startReview(deposit.id))                     // Tx 2
	step(t, d.Pool, decide(deposit.id, compliance.Approved(""))) // Tx 3, then the crash

	// This time the dice say reject: the recorded approval must stand.
	d.Decide = reject
	if got := Handle(t.Context(), d, message(body, "crashed", 2)); got != consumer.Applied {
		t.Fatalf("outcome %s", got)
	}
	if s := statusOf(t, d.Pool, deposit.id); s != "credited" {
		t.Errorf("status %s", s)
	}
	if h := historyOf(t, d.Pool, deposit.id); slices.Contains(h, "rejected") {
		t.Errorf("history %v", h)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book", "credit"}) {
		t.Errorf("movements %v", s)
	}
}

func TestTerminatesAnEventThatIsNotValidJSONLoggingTheDeliveryWithoutAnEventID(t *testing.T) {
	d := testDeps(t)
	msgID := "garbage-" + ids.New[ids.Event]().TypeID()
	m := message("{not json", msgID, 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Invalid, "term")
	var outcome string
	var eventID *string
	err := d.Pool.QueryRow(t.Context(),
		`SELECT outcome, event_id::text FROM event_deliveries WHERE nats_msg_id = $1`, msgID).
		Scan(&outcome, &eventID)
	if err != nil || outcome != "invalid" || eventID != nil {
		t.Errorf("row: %s %v %v", outcome, eventID, err)
	}
}

func TestTerminatesAnEventThatFailsValidationKeepingItsEventIDInTheLog(t *testing.T) {
	d := testDeps(t)
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	deposit := newDeposit(t, d.Pool)
	body, eventID := eventFor(deposit)
	broken := strings.Replace(body, `"amount_minor":"1250"`, `"amount_minor":1250`, 1)
	m := message(broken, "msg-1", 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Invalid, "term")
	rows := deliveriesOf(t, d.Pool, eventID)
	if len(rows) != 1 || rows[0].outcome != "invalid" || *rows[0].natsMsgID != "msg-1" {
		t.Errorf("deliveries %+v", rows)
	}
	line := logs.Line("invalid deposit.created event; terminating it")
	if line["event_id"] != eventID.TypeID() {
		t.Errorf("line = %v", line)
	}
	if s := statusOf(t, d.Pool, deposit.id); s != "created" {
		t.Errorf("status %s", s)
	}
}

func TestTerminatesAnEventForADepositThatDoesNotExist(t *testing.T) {
	d := testDeps(t)
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	ghost := newDeposit(t, d.Pool)
	ghost.id = ids.New[ids.Deposit]()
	body, eventID := eventFor(ghost)
	m := message(body, "msg-1", 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Invalid, "term")
	if rows := deliveriesOf(t, d.Pool, eventID); len(rows) != 1 || rows[0].outcome != "invalid" {
		t.Errorf("deliveries %+v", rows)
	}
	line := logs.Line("deposit.created event for an unknown deposit; terminating it")
	if line == nil || line["level"] != "error" || line["deposit_id"] != ghost.id.TypeID() {
		t.Errorf("line = %v", line)
	}
}

// The transient failure is injected through Deps.Decide, which panics as Node's test throws from
// decide(): the lifecycle recovers the panic and classifies it, not a bug, as transient.
func TestNaksAfterAnUnexpectedFailureAndTheRedeliveryCompletesFromWhereItStopped(t *testing.T) {
	d := testDeps(t)
	deposit := newDeposit(t, d.Pool)
	body, eventID := eventFor(deposit)
	failing := d
	failing.Decide = func() compliance.Decision { panic("compliance service unavailable") }
	first := message(body, "m", 1)
	settled(t, Handle(t.Context(), failing, first), first, consumer.Failed, "nak")
	// Tx 1 and 2 committed before the failure.
	if s := statusOf(t, d.Pool, deposit.id); s != "under_review" {
		t.Errorf("status after the failure %s", s)
	}

	if got := Handle(t.Context(), d, message(body, "m", 2)); got != consumer.Applied {
		t.Fatalf("redelivery: %s", got)
	}
	if s := statusOf(t, d.Pool, deposit.id); s != "credited" {
		t.Errorf("status %s", s)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book", "credit"}) {
		t.Errorf("movements %v", s)
	}
	o := outcomes(deliveriesOf(t, d.Pool, eventID))
	if !slices.Equal(o, []string{"failed", "applied"}) {
		t.Errorf("deliveries %v", o)
	}
}

func TestNaksWithoutFailingWhenTheDeliveryCannotEvenBeRecorded(t *testing.T) {
	d := testDeps(t)
	unreachable, err := pgxpool.New(t.Context(),
		"postgres://ledger:ledger@localhost:1/ledger_go_test?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer unreachable.Close()
	d.Pool = unreachable
	m := message("{}", "m", 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Failed, "nak")
}

func TestSignalsWorkingDuringTheReviewSoTheMessageIsNotRedeliveredMeanwhile(t *testing.T) {
	d := testDeps(t)
	d.ReviewDelay, d.WorkingInterval = 50*time.Millisecond, 20*time.Millisecond
	deposit := newDeposit(t, d.Pool)
	body, _ := eventFor(deposit)
	m := message(body, "m", 1)
	Handle(t.Context(), d, m)
	// 50 ms in stretches of at most 20 ms: three signals, then the ack.
	if want := []string{"working", "working", "working", "ack"}; !slices.Equal(m.Calls(), want) {
		t.Errorf("calls %v, want %v", m.Calls(), want)
	}
}

func TestSkipsTheReviewDelayForADuplicateAndForAnUnknownDeposit(t *testing.T) {
	d := testDeps(t)
	slow := d
	slow.ReviewDelay, slow.WorkingInterval = 300*time.Millisecond, 100*time.Millisecond
	deposit := newDeposit(t, d.Pool)
	body, _ := eventFor(deposit)
	if got := Handle(t.Context(), d, message(body, "first", 1)); got != consumer.Applied {
		t.Fatalf("first: %s", got)
	}

	duplicate := message(body, "second", 1)
	started := time.Now()
	if got := Handle(t.Context(), slow, duplicate); got != consumer.Duplicate {
		t.Fatalf("duplicate: %s", got)
	}
	// No Working: it didn't wait.
	if !slices.Equal(duplicate.Calls(), []string{"ack"}) ||
		time.Since(started) >= slow.ReviewDelay { // a review would take at least this long
		t.Errorf("calls %v after %v", duplicate.Calls(), time.Since(started))
	}

	ghost := deposit
	ghost.id = ids.New[ids.Deposit]()
	ghostBody, _ := eventFor(ghost)
	m := message(ghostBody, "ghost", 1)
	settled(t, Handle(t.Context(), slow, m), m, consumer.Invalid, "term")
}

// The ledger already holds the booking but the deposit is still created: Book returns a
// LedgerInconsistencyError, which no retry can fix.
func TestTerminatesInsteadOfRetryingWhenAnInvariantIsViolatedLoggingItAsFatal(t *testing.T) {
	d := testDeps(t)
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	deposit := newDeposit(t, d.Pool)
	step(t, d.Pool, func(ctx context.Context, tx db.Tx) (ledger.MoveResult, error) {
		return ledger.Move(ctx, tx, ledger.Movement{
			Account: deposit.account.ID, From: ledger.BucketExternal, To: ledger.BucketPendingIn,
			Amount: deposit.amount, Ref: ledger.DepositReference(deposit.id, ledger.StepBook),
		})
	})
	body, eventID := eventFor(deposit)
	m := message(body, "m", 1)

	settled(t, Handle(t.Context(), d, m), m, consumer.Failed, "term")
	if s := statusOf(t, d.Pool, deposit.id); s != "created" {
		t.Errorf("status %s", s)
	}
	if o := outcomes(deliveriesOf(t, d.Pool, eventID)); !slices.Equal(o, []string{"failed"}) {
		t.Errorf("deliveries %v", o)
	}
	fatal := logs.Lines("fatal")
	if len(fatal) != 1 || fatal[0]["alert"] != true ||
		fatal[0]["msg"] != "invariant violated; terminating the message" {
		t.Errorf("fatal lines %v", fatal)
	}
}

// Booked and approved, then the balance corrupted so crediting would overdraw pending_in: only a
// bug could get here, so it must not be retried.
func TestTerminatesOnAnOverdraftErrorRatherThanRetryingIt(t *testing.T) {
	d := testDeps(t)
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	deposit := newDeposit(t, d.Pool)
	step(t, d.Pool, book(deposit.id))
	step(t, d.Pool, startReview(deposit.id))
	step(t, d.Pool, decide(deposit.id, compliance.Approved("")))
	_, err := d.Pool.Exec(t.Context(),
		`UPDATE account_balances SET pending_in = 0 WHERE account_id = $1`, deposit.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := eventFor(deposit)
	m := message(body, "m", 1)

	settled(t, Handle(t.Context(), d, m), m, consumer.Failed, "term")
	if s := statusOf(t, d.Pool, deposit.id); s != "approved" {
		t.Errorf("status %s", s)
	}
	fatal := logs.Lines("fatal")
	if len(fatal) != 1 || fatal[0]["alert"] != true {
		t.Fatalf("fatal lines %v", fatal)
	}
	// The error is wrapped on its way up; the OverdraftError's text is in its message.
	logged, _ := fatal[0]["err"].(map[string]any)
	if message, _ := logged["message"].(string); !strings.Contains(message, "would overdraw pending_in") {
		t.Errorf("err = %v", fatal[0]["err"])
	}
}

// failingFinish is the pool with every delivery-log UPDATE failing (Node's failingDeliveryFinish).
type failingFinish struct{ db.Querier }

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

// Signal vs record: a stop is signalled first, so a failed write to the delivery log doesn't turn
// "never deliver again" into a redelivery.
func TestTerminatesAnInvalidMessageEvenWhenItsOutcomeCannotBeRecorded(t *testing.T) {
	d := testDeps(t)
	d.deliveryLog = failingFinish{d.Pool}
	m := message("not json", "m", 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Invalid, "term")
}

// Decision 59: when shutdown's grace period ends mid-review, the handler's context is cancelled,
// the review stops at once, and the lifecycle naks it as transient; nothing after the review ran.
func TestACancelledReviewIsNakedAsTransientWithNothingPosted(t *testing.T) {
	d := testDeps(t)
	d.ReviewDelay, d.WorkingInterval = 10*time.Second, 50*time.Millisecond
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	deposit := newDeposit(t, d.Pool)
	body, eventID := eventFor(deposit)
	ctx, cancel := context.WithCancel(t.Context())
	// The grace period ends mid-review: on the review's first Working, which proves it has started
	// (a timer could fire before the review, under load, while booking).
	m := &cancelOnWorking{Message: message(body, "m", 1), cancel: cancel}
	started := time.Now()
	if got := Handle(ctx, d, m); got != consumer.Failed {
		t.Fatalf("outcome %s", got)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the review ran on for %v after cancellation", elapsed)
	}
	calls := m.Calls()
	if len(calls) < 2 || calls[0] != "working" || calls[len(calls)-1] != "nak" ||
		slices.Contains(calls, "ack") {
		t.Errorf("calls %v", calls)
	}
	if s := statusOf(t, d.Pool, deposit.id); s != "under_review" {
		t.Errorf("status %s", s)
	}
	if s := movementSteps(t, d.Pool, deposit.id); !slices.Equal(s, []string{"book"}) {
		t.Errorf("movements %v", s)
	}
	// Recorded failed despite the cancelled context (the outcome writes aren't cancelled with it).
	if o := outcomes(deliveriesOf(t, d.Pool, eventID)); !slices.Equal(o, []string{"failed"}) {
		t.Errorf("deliveries %v", o)
	}
	line := logs.Line("processing failed; will retry")
	err, _ := line["err"].(map[string]any)
	if err == nil || !strings.Contains(err["message"].(string), "context canceled") {
		t.Errorf("line = %v", line)
	}
}

// cancelOnWorking is a fake message that cancels a context on the first Working. It embeds the
// fake (its methods become this type's) and overrides Working alone.
type cancelOnWorking struct {
	*consumertest.Message
	cancel context.CancelFunc
}

func (m *cancelOnWorking) Working() error {
	defer m.cancel()
	return m.Message.Working()
}

func TestDecideRandomlyApprovesBelowTheRateAndRejectsAtOrAboveItWithAReason(t *testing.T) {
	d := compliance.DecideRandomly(0.8, func() float64 { return 0.79 })
	if d.Outcome() != "approved" || d.Reason() != "" {
		t.Errorf("0.79: %s %q", d.Outcome(), d.Reason())
	}
	d = compliance.DecideRandomly(0.8, func() float64 { return 0.8 })
	if d.Outcome() != "rejected" || d.Reason() != compliance.RejectionReason {
		t.Errorf("0.8: %s %q", d.Outcome(), d.Reason())
	}
}

// Working is best-effort: one that can't be sent is logged, and the review and the delivery carry
// on.
func TestAWorkingSignalThatCantBeSentDoesntStopTheReview(t *testing.T) {
	d := testDeps(t)
	d.ReviewDelay, d.WorkingInterval = 40*time.Millisecond, 20*time.Millisecond
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	deposit := newDeposit(t, d.Pool)
	body, _ := eventFor(deposit)
	m := message(body, "m", 1)
	m.FailWorking(errors.New("nats: connection closed"))
	settled(t, Handle(t.Context(), d, m), m, consumer.Applied, "working", "working", "ack")
	if s := statusOf(t, d.Pool, deposit.id); s != "credited" {
		t.Errorf("status %s", s)
	}
	if len(logs.Lines("warn")) != 2 || logs.Line("could not signal working; carrying on with the review") == nil {
		t.Errorf("warn lines %v", logs.Lines("warn"))
	}
}

// A zero WorkingInterval is one stretch, not a loop of zero-length waits.
func TestAZeroWorkingIntervalReviewsInOneStretch(t *testing.T) {
	d := testDeps(t)
	d.ReviewDelay, d.WorkingInterval = 30*time.Millisecond, 0
	deposit := newDeposit(t, d.Pool)
	body, _ := eventFor(deposit)
	m := message(body, "m", 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Applied, "working", "ack")
}
