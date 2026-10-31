package submitter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/consumertest"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/requests"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// The submitter's handler against the test database, with fake messages and a fake provider: the
// cases of node/test/submitter/handler.test.ts. A message here is what JetStream would deliver,
// and "redelivery" is calling the handler again. In the package itself (not submitter_test) so one
// case can make the delivery log's writes fail through the unexported Deps.deliveryLog.

// fakeProvider answers with its outcomes in turn (the last one repeats), recording the withdrawals
// it was asked to submit. during, if set, runs inside the call (the provider's update racing it).
type fakeProvider struct {
	mu       sync.Mutex
	outcomes []Outcome
	calls    []withdrawals.Withdrawal
	during   func(w withdrawals.Withdrawal)
}

func (p *fakeProvider) SubmitTransfer(
	_ context.Context,
	w withdrawals.Withdrawal,
) Outcome {
	p.mu.Lock()
	p.calls = append(p.calls, w)
	n := len(p.calls)
	p.mu.Unlock()
	if p.during != nil {
		p.during(w)
	}
	return p.outcomes[min(n, len(p.outcomes))-1]
}

func scripted(outcomes ...Outcome) *fakeProvider {
	return &fakeProvider{outcomes: outcomes}
}

var (
	accepted = Outcome{Kind: Accepted, TransferID: "tr_1"}
	refusal  = Outcome{Kind: Refused, Reason: "creditor account closed"}
)

func testDeps(t *testing.T, p Provider) Deps {
	t.Helper()
	return Deps{
		Pool:     testdb.Pool(t),
		Logger:   slog.New(slog.DiscardHandler),
		Provider: p,
		Retry:    backoff.Backoff{Base: time.Second, Max: 60 * time.Second},
	}
}

func message(body string, attempt int) *consumertest.Message {
	return consumertest.NewMessage(body, "wd.held", attempt)
}

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

func inTxT[T any](
	t *testing.T,
	pool *pgxpool.Pool,
	f func(ctx context.Context, tx db.Tx) (T, error),
) T {
	t.Helper()
	v, err := db.InTxValue(context.Background(), pool, func(tx db.Tx) (T, error) {
		return f(context.Background(), tx)
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type held struct {
	withdrawal withdrawals.Withdrawal
	body       string
	eventID    ids.EventID
}

// heldWithdrawal is a held withdrawal of 400 from an account funded with 1000 (the real way: a
// deposit credited, then the withdrawal requested), and its event as published.
func heldWithdrawal(t *testing.T, pool *pgxpool.Pool) held {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	deposit := testdb.NewDeposit(t, pool, account, 1000)
	inTxT(t, pool, func(ctx context.Context, tx db.Tx) (ledger.Posted, error) {
		if _, err := ledger.Book(ctx, tx, deposit); err != nil {
			return "", err
		}
		for _, tr := range []deposits.Transition{deposits.StartReview, deposits.Approve} {
			if _, _, err := deposits.TransitionStatus(ctx, tx, deposit, tr, ""); err != nil {
				return "", err
			}
		}
		return ledger.PostDecision(ctx, tx, deposit)
	})
	remittance := "test withdrawal"
	result := inTxT(t, pool, func(ctx context.Context, tx db.Tx) (requests.Result, error) {
		return requests.RequestWithdrawal(ctx, tx, withdrawals.NewWithdrawal{
			Account: account.ID, IdempotencyKey: ids.New[ids.Event]().TypeID(), Amount: 400,
			CreditorIBAN: "DE75512108001245126199", CreditorName: "Jane Doe",
			RemittanceInfo: &remittance,
		})
	})
	w := result.Withdrawal
	eventID := ids.New[ids.Event]()
	body, err := json.Marshal(events.WithdrawalHeldV1{
		EventID: eventID, EventType: events.SubjectWithdrawalHeld, SchemaVersion: 1,
		WithdrawalID: w.ID, AccountID: w.Account, AmountMinor: w.Amount, Currency: w.Currency,
		OccurredAt: timestamp.Time(w.CreatedAt.Truncate(time.Millisecond)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return held{withdrawal: w, body: string(body), eventID: eventID}
}

func get(t *testing.T, pool *pgxpool.Pool, id ids.WithdrawalID) withdrawals.Withdrawal {
	t.Helper()
	w, found, err := withdrawals.Get(context.Background(), pool, id)
	if err != nil || !found {
		t.Fatalf("withdrawal %v: %v %v", id, found, err)
	}
	return w
}

type balances struct{ pendingIn, available, held money.Amount }

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

func deliveriesOf(t *testing.T, pool *pgxpool.Pool, eventID ids.EventID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT consumer || ':' || outcome FROM event_deliveries WHERE event_id = $1 ORDER BY id`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		found = append(found, s)
	}
	return found
}

// expectDeliveries fails the test unless the event's delivery rows are want ("consumer:outcome").
func expectDeliveries(t *testing.T, pool *pgxpool.Pool, eventID ids.EventID, want ...string) {
	t.Helper()
	if got := deliveriesOf(t, pool, eventID); !slices.Equal(got, want) {
		t.Errorf("deliveries %v, want %v", got, want)
	}
}

// transferID is the withdrawal's provider transfer ID, "" for none.
func transferID(w withdrawals.Withdrawal) withdrawals.TransferID {
	if w.ProviderTransferID == nil {
		return ""
	}
	return *w.ProviderTransferID
}

func TestSubmitsAHeldWithdrawalBuiltFromTheStoredWithdrawal(t *testing.T) {
	p := scripted(accepted)
	d := testDeps(t, p)
	h := heldWithdrawal(t, d.Pool)
	m := message(h.body, 1)
	// Working before the call resets ack_wait, so the call starts with the full lease.
	settled(t, Handle(t.Context(), d, m), m, consumer.Applied, "working", "ack")
	got := get(t, d.Pool, h.withdrawal.ID)
	if got.Status != withdrawals.StatusSubmitted || transferID(got) != "tr_1" {
		t.Errorf("withdrawal %+v", got)
	}
	// The provider got the stored withdrawal (it sends its TypeID as the key, its fields as the
	// body; internal/provider's tests check the wire).
	if len(p.calls) != 1 || !reflect.DeepEqual(p.calls[0], h.withdrawal) {
		t.Errorf("calls %+v\nwant %+v", p.calls, h.withdrawal)
	}
	expectDeliveries(t, d.Pool, h.eventID, "submitter:applied")
}

func TestRecordsNothingOnARedeliveryAfterTheSubmissionWasRecorded(t *testing.T) {
	p := scripted(accepted)
	d := testDeps(t, p)
	h := heldWithdrawal(t, d.Pool)
	Handle(t.Context(), d, message(h.body, 1))
	// Redelivered as if the ack was lost: already submitted, so no call.
	again := message(h.body, 2)
	settled(t, Handle(t.Context(), d, again), again, consumer.Duplicate, "ack")
	if len(p.calls) != 1 {
		t.Errorf("%d calls", len(p.calls))
	}
	expectDeliveries(t, d.Pool, h.eventID, "submitter:applied", "submitter:duplicate")
}

// The provider's update settles the withdrawal while the call is in flight (decision 27):
// MarkSubmitted skips, a duplicate.
func TestTreatsAnAcceptanceForAWithdrawalSettledMeanwhileAsADuplicate(t *testing.T) {
	p := scripted(accepted)
	d := testDeps(t, p)
	h := heldWithdrawal(t, d.Pool)
	p.during = func(w withdrawals.Withdrawal) {
		inTxT(t, d.Pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
			return ledger.Execute(ctx, tx, w.ID, "tr_1")
		})
	}
	m := message(h.body, 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Duplicate, "working", "ack")
	if s := get(t, d.Pool, h.withdrawal.ID).Status; s != withdrawals.StatusExecuted {
		t.Errorf("status %s", s)
	}
}

func TestMakesNoCallForAWithdrawalNoLongerHeld(t *testing.T) {
	p := scripted(accepted)
	d := testDeps(t, p)
	h := heldWithdrawal(t, d.Pool)
	inTxT(t, d.Pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
		return ledger.Execute(ctx, tx, h.withdrawal.ID, "tr_early")
	})
	m := message(h.body, 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Duplicate, "ack")
	if len(p.calls) != 0 {
		t.Errorf("%d calls", len(p.calls))
	}
}

func TestReleasesTheFundsOnARefusal(t *testing.T) {
	d := testDeps(t, scripted(refusal))
	h := heldWithdrawal(t, d.Pool)
	m := message(h.body, 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Applied, "working", "ack")
	got := get(t, d.Pool, h.withdrawal.ID)
	if got.Status != withdrawals.StatusFailed || got.FailureReason == nil ||
		*got.FailureReason != "creditor account closed" {
		t.Errorf("withdrawal %+v", got)
	}
	if b := balancesOf(t, d.Pool, h.withdrawal.Account); b != (balances{0, 1000, 0}) {
		t.Errorf("balances %+v", b)
	}
}

func TestNaksAnUnknownOutcomeWithAGrowingDelayAndTheRedeliverySubmitsIt(t *testing.T) {
	p := scripted(
		Outcome{Kind: Unknown, Cause: "timeout"},
		Outcome{Kind: Unknown, Cause: "provider answered 503"},
		accepted,
	)
	d := testDeps(t, p)
	h := heldWithdrawal(t, d.Pool)
	first, second := message(h.body, 1), message(h.body, 2)
	settled(t, Handle(t.Context(), d, first), first, consumer.Failed, "working", "nak")
	settled(t, Handle(t.Context(), d, second), second, consumer.Failed, "working", "nak")
	if first.NakDelay() != time.Second || second.NakDelay() != 2*time.Second {
		t.Errorf("delays %v, %v", first.NakDelay(), second.NakDelay())
	}
	if s := get(t, d.Pool, h.withdrawal.ID).Status; s != withdrawals.StatusHeld {
		t.Errorf("status %s", s)
	}
	if b := balancesOf(t, d.Pool, h.withdrawal.Account); b.held != 400 {
		t.Errorf("balances %+v", b)
	}
	third := message(h.body, 3)
	settled(t, Handle(t.Context(), d, third), third, consumer.Applied, "working", "ack")
	if s := get(t, d.Pool, h.withdrawal.ID).Status; s != withdrawals.StatusSubmitted {
		t.Errorf("status %s", s)
	}
	// The same withdrawal (so the same key) every time; never a new one.
	for _, c := range p.calls {
		if c.ID != h.withdrawal.ID {
			t.Errorf("a call for %v", c.ID)
		}
	}
	want := []string{"submitter:failed", "submitter:failed", "submitter:applied"}
	if got := deliveriesOf(t, d.Pool, h.eventID); !slices.Equal(got, want) {
		t.Errorf("deliveries %v", got)
	}
}

func TestTreatsInvalidAndKeyConflictAsOurBug(t *testing.T) {
	for name, outcome := range map[string]Outcome{
		"invalid (400)": {
			Kind: Invalid, Fields: []string{"creditor_name"}, BodyBytes: 120,
		},
		"key conflict (409)": {Kind: KeyConflict},
	} {
		d := testDeps(t, scripted(outcome))
		logger, logs := consumertest.NewLogger(t)
		d.Logger = logger
		h := heldWithdrawal(t, d.Pool)
		m := message(h.body, 1)
		settled(t, Handle(t.Context(), d, m), m, consumer.Failed, "working", "term")
		if s := get(t, d.Pool, h.withdrawal.ID).Status; s != withdrawals.StatusHeld {
			t.Errorf("%s: status %s", name, s)
		}
		if b := balancesOf(t, d.Pool, h.withdrawal.Account); b != (balances{0, 600, 400}) {
			t.Errorf("%s: balances %+v", name, b)
		}
		fatal := logs.Lines("fatal")
		if len(fatal) != 1 || fatal[0]["alert"] != true ||
			!strings.HasSuffix(fatal[0]["msg"].(string),
				"; terminating the message, withdrawal left held") {
			t.Errorf("%s: fatal lines %v", name, fatal)
		}
		// Node checks reconciliation lists it as stuck (Node only, decision 50): it is left held,
		// its delivery failed, for a person to resolve.
		expectDeliveries(t, d.Pool, h.eventID, "submitter:failed")
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

// Signal vs record: a stop is signalled first, so a failed write to the delivery log can't leave
// the message to be redelivered and the alert repeated.
func TestTerminatesABugOutcomeEvenWhenItsOutcomeCannotBeRecorded(t *testing.T) {
	d := testDeps(t, scripted(Outcome{Kind: KeyConflict}))
	d.deliveryLog = failingFinish{d.Pool}
	h := heldWithdrawal(t, d.Pool)
	m := message(h.body, 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Failed, "working", "term")
	if s := get(t, d.Pool, h.withdrawal.ID).Status; s != withdrawals.StatusHeld {
		t.Errorf("status %s", s)
	}
	// The trace left behind: a delivery still in_progress, which reconciliation reports.
	expectDeliveries(t, d.Pool, h.eventID, "submitter:in_progress")
}

// The provider's update stored tr_A (and executed it), then this submission gets tr_B.
func TestTreatsATransferIDMismatchFromMarkSubmittedAsAnInvariantViolation(t *testing.T) {
	p := scripted(Outcome{Kind: Accepted, TransferID: "tr_B"})
	d := testDeps(t, p)
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	h := heldWithdrawal(t, d.Pool)
	p.during = func(w withdrawals.Withdrawal) {
		inTxT(t, d.Pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
			return ledger.Execute(ctx, tx, w.ID, "tr_A")
		})
	}
	m := message(h.body, 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Failed, "working", "term")
	// Both transfer IDs are in the logged error: its message names them (Node's logger copies the
	// error's fields instead).
	fatal := logs.Lines("fatal")
	err, _ := fatal[0]["err"].(map[string]any)
	const invariant = "invariant violated; terminating the message"
	if len(fatal) != 1 || fatal[0]["alert"] != true || fatal[0]["msg"] != invariant ||
		!strings.Contains(err["message"].(string), "tr_A") ||
		!strings.Contains(err["message"].(string), "tr_B") {
		t.Errorf("fatal lines %v", fatal)
	}
	if got := get(t, d.Pool, h.withdrawal.ID); transferID(got) != "tr_A" {
		t.Errorf("withdrawal %+v", got)
	}
}

// A release movement already exists for a withdrawal still held: the two guards disagree, so
// Release returns a LedgerInconsistencyError. Retrying can't fix it.
func TestTerminatesALedgerInvariantViolationWithAnAlert(t *testing.T) {
	d := testDeps(t, scripted(refusal))
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	h := heldWithdrawal(t, d.Pool)
	inTxT(t, d.Pool, func(ctx context.Context, tx db.Tx) (ledger.MoveResult, error) {
		return ledger.Move(ctx, tx, ledger.Movement{
			Account: h.withdrawal.Account, From: ledger.BucketHeld, To: ledger.BucketAvailable,
			Amount: h.withdrawal.Amount,
			Ref:    ledger.WithdrawalReference(h.withdrawal.ID, ledger.StepRelease),
		})
	})
	before := balancesOf(t, d.Pool, h.withdrawal.Account)
	m := message(h.body, 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Failed, "working", "term")
	fatal := logs.Lines("fatal")
	if len(fatal) != 1 || fatal[0]["alert"] != true ||
		fatal[0]["msg"] != "invariant violated; terminating the message" {
		t.Errorf("fatal lines %v", fatal)
	}
	// The transition rolled back with the failed movement.
	if s := get(t, d.Pool, h.withdrawal.ID).Status; s != withdrawals.StatusHeld {
		t.Errorf("status %s", s)
	}
	if b := balancesOf(t, d.Pool, h.withdrawal.Account); b != before {
		t.Errorf("balances %+v, before %+v", b, before)
	}
}

// The database goes away after an accepted call (a crash between the call and the transaction
// looks the same): the fake provider closes the handler's pool during the call, so the read before
// it worked and the recording after it fails. Nak; the redelivery sends the same request and
// records it.
func TestNaksATransientFailureAfterAnAcceptedCallAndTheRedeliveryRecordsIt(t *testing.T) {
	p := scripted(accepted)
	d := testDeps(t, p)
	h := heldWithdrawal(t, d.Pool)
	broken := d
	broken.Pool = testdb.Pool(t) // a pool of its own, closed during the call
	broken.deliveryLog = d.Pool  // the delivery log still works
	p.during = func(withdrawals.Withdrawal) { broken.Pool.Close() }
	m := message(h.body, 1)
	settled(t, Handle(t.Context(), broken, m), m, consumer.Failed, "working", "nak")
	if s := get(t, d.Pool, h.withdrawal.ID).Status; s != withdrawals.StatusHeld {
		t.Errorf("status %s", s)
	}

	p.during = nil
	again := message(h.body, 2)
	settled(t, Handle(t.Context(), d, again), again, consumer.Applied, "working", "ack")
	if got := get(t, d.Pool, h.withdrawal.ID); transferID(got) != "tr_1" {
		t.Errorf("withdrawal %+v", got)
	}
	// Accepted twice, with the identical withdrawal (so the same key and body) both times.
	if len(p.calls) != 2 || !reflect.DeepEqual(p.calls[0], p.calls[1]) {
		t.Errorf("calls %+v", p.calls)
	}
}

func TestTerminatesAnInvalidEvent(t *testing.T) {
	d := testDeps(t, scripted(accepted))
	for name, body := range map[string]string{
		"not JSON":                    "not json",
		"not a withdrawal.held event": `{"event_type":"deposit.created"}`,
	} {
		m := message(body, 1)
		got := Handle(t.Context(), d, m)
		if got != consumer.Invalid || !slices.Equal(m.Calls(), []string{"term"}) {
			t.Errorf("%s: %s %v", name, got, m.Calls())
		}
	}
}

func TestTerminatesAnEventForAnUnknownWithdrawal(t *testing.T) {
	p := scripted(accepted)
	d := testDeps(t, p)
	h := heldWithdrawal(t, d.Pool)
	eventID := ids.New[ids.Event]()
	body, _ := json.Marshal(events.WithdrawalHeldV1{
		EventID: eventID, EventType: events.SubjectWithdrawalHeld, SchemaVersion: 1,
		WithdrawalID: ids.New[ids.Withdrawal](), AccountID: h.withdrawal.Account, AmountMinor: 400,
		Currency: "EUR", OccurredAt: timestamp.Time(time.Now().UTC().Truncate(time.Millisecond)),
	})
	m := message(string(body), 1)
	settled(t, Handle(t.Context(), d, m), m, consumer.Invalid, "term")
	if len(p.calls) != 0 {
		t.Errorf("%d calls", len(p.calls))
	}
	if got := deliveriesOf(t, d.Pool, eventID); !slices.Equal(got, []string{"submitter:invalid"}) {
		t.Errorf("deliveries %v", got)
	}
}

func TestLogsNoNameIBANOrRemittanceText(t *testing.T) {
	d := testDeps(t, scripted(accepted))
	logger, logs := consumertest.NewLogger(t)
	d.Logger = logger
	h := heldWithdrawal(t, d.Pool)
	Handle(t.Context(), d, message(h.body, 1))
	everything, _ := json.Marshal(logs.Lines(""))
	for _, personal := range []string{"Jane Doe", "test withdrawal", "DE75512108001245126199"} {
		if strings.Contains(string(everything), personal) {
			t.Errorf("the logs hold %q", personal)
		}
	}
	if line := logs.Line("withdrawal submitted"); line == nil || line["transfer_id"] != "tr_1" ||
		line["submitted"] != "applied" || line["withdrawal_id"] != h.withdrawal.ID.TypeID() {
		t.Errorf("line = %v", line)
	}
}
