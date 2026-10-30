package ledger_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The deposit lifecycle through the postings and the compliance transitions: the cases of
// node/test/ledger/lifecycle.test.ts. Go has no compliance package yet (step 6), so the review and
// decision are deposits.TransitionStatus with the compliance transitions, which is all Node's
// startReview and decide do. Each test has its own account and deposit.

type historyRow struct {
	from, to, reason string // "" for NULL
}

func history(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID) []historyRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT coalesce(from_status, ''), to_status, coalesce(reason, '')
FROM deposit_status_history WHERE deposit_id = $1 ORDER BY id`, deposit)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []historyRow
	for rows.Next() {
		var r historyRow
		if err := rows.Scan(&r.from, &r.to, &r.reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func statusOf(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM deposits WHERE id = $1`, deposit).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func newDeposit(t *testing.T, pool *pgxpool.Pool) (ids.AccountID, ids.DepositID) {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	return account.ID, testdb.NewDeposit(t, pool, account, 1250)
}

// posting runs one posting function in its own transaction.
func posting(pool *pgxpool.Pool, fn func(context.Context, db.Tx, ids.DepositID) (ledger.Result, error), deposit ids.DepositID) (ledger.Result, error) {
	return inTx(pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) { return fn(ctx, tx, deposit) })
}

// transition runs a compliance transition in its own transaction, as "applied" or "skipped".
func transition(pool *pgxpool.Pool, deposit ids.DepositID, tr deposits.Transition, reason string) (ledger.Result, error) {
	return inTx(pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
		_, applied, err := deposits.TransitionStatus(ctx, tx, deposit, tr, reason)
		if err != nil || !applied {
			return ledger.Skipped, err
		}
		return ledger.Applied, nil
	})
}

func postDecision(pool *pgxpool.Pool, deposit ids.DepositID) (ledger.Posted, error) {
	return inTx(pool, func(ctx context.Context, tx db.Tx) (ledger.Posted, error) {
		return ledger.PostDecision(ctx, tx, deposit)
	})
}

// must returns v, or panics if err isn't nil: for the steps of a test whose own outcome isn't
// what's being checked (a panic fails the test, with a stack trace). Go only lets a call that
// returns several values be passed as all of a function's arguments, so must can't also take t;
// this is the standard library's template.Must pattern.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestApprovedIsBookedThenCredited(t *testing.T) {
	pool := testdb.Pool(t)
	account, deposit := newDeposit(t, pool)

	if r := must(posting(pool, ledger.Book, deposit)); r != ledger.Applied {
		t.Fatalf("Book = %v", r)
	}
	if s := statusOf(t, pool, deposit); s != "received" {
		t.Errorf("status = %s, want received", s)
	}
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("after booking: %+v", got)
	}
	must(transition(pool, deposit, deposits.StartReview, ""))
	if r := must(transition(pool, deposit, deposits.Approve, "")); r != ledger.Applied {
		t.Fatalf("Approve = %v", r)
	}
	// Decisions move no money.
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("after the decision: %+v", got)
	}
	if p := must(postDecision(pool, deposit)); p != ledger.PostedCredit {
		t.Fatalf("PostDecision = %v, want credited", p)
	}
	if got := balancesOf(t, pool, account); got != (balances{0, 1250, 0}) {
		t.Errorf("after crediting: %+v", got)
	}
	want := []historyRow{
		{"", "created", ""},
		{"created", "received", ""},
		{"received", "under_review", ""},
		{"under_review", "approved", ""},
		{"approved", "credited", ""},
	}
	if got := history(t, pool, deposit); !slices.Equal(got, want) {
		t.Errorf("history = %v\nwant      %v", got, want)
	}
}

func TestRejectedIsBookedThenReturnedWithTheReason(t *testing.T) {
	pool := testdb.Pool(t)
	account, deposit := newDeposit(t, pool)
	must(posting(pool, ledger.Book, deposit))
	must(transition(pool, deposit, deposits.StartReview, ""))
	if r := must(transition(pool, deposit, deposits.Reject, "sanctions screening hit")); r != ledger.Applied {
		t.Fatalf("Reject = %v", r)
	}
	if p := must(postDecision(pool, deposit)); p != ledger.PostedReturn {
		t.Fatalf("PostDecision = %v, want returned", p)
	}
	if s := statusOf(t, pool, deposit); s != "returned" {
		t.Errorf("status = %s, want returned", s)
	}
	if got := balancesOf(t, pool, account); got != (balances{}) {
		t.Errorf("balances = %+v", got)
	}
	if got := history(t, pool, deposit)[3]; got != (historyRow{"under_review", "rejected", "sanctions screening hit"}) {
		t.Errorf("decision history row = %+v", got)
	}
}

func TestSkipsEveryTransitionWhenRepeated(t *testing.T) {
	pool := testdb.Pool(t)
	account, deposit := newDeposit(t, pool)
	steps := []struct {
		name string
		run  func() (ledger.Result, error)
	}{
		{"book", func() (ledger.Result, error) { return posting(pool, ledger.Book, deposit) }},
		{"start review", func() (ledger.Result, error) { return transition(pool, deposit, deposits.StartReview, "") }},
		{"approve", func() (ledger.Result, error) { return transition(pool, deposit, deposits.Approve, "") }},
		{"credit", func() (ledger.Result, error) { return posting(pool, ledger.Credit, deposit) }},
	}
	for _, s := range steps {
		if r := must(s.run()); r != ledger.Applied {
			t.Fatalf("%s = %v, want applied", s.name, r)
		}
		if r := must(s.run()); r != ledger.Skipped {
			t.Fatalf("%s again = %v, want skipped", s.name, r)
		}
	}
	// Every step once more, after the deposit is credited.
	for _, s := range steps {
		if r := must(s.run()); r != ledger.Skipped {
			t.Errorf("%s after credit = %v, want skipped", s.name, r)
		}
	}
	if p := must(postDecision(pool, deposit)); p != ledger.PostedSkipped {
		t.Errorf("PostDecision = %v, want skipped", p)
	}
	if got := balancesOf(t, pool, account); got != (balances{0, 1250, 0}) {
		t.Errorf("balances = %+v", got)
	}
	if n := len(history(t, pool, deposit)); n != 5 {
		t.Errorf("%d history rows, want 5", n)
	}
}

func TestSkipsTransitionsOutOfOrder(t *testing.T) {
	pool := testdb.Pool(t)
	account, deposit := newDeposit(t, pool)
	for name, run := range map[string]func() (ledger.Result, error){
		"start review":     func() (ledger.Result, error) { return transition(pool, deposit, deposits.StartReview, "") },
		"approve":          func() (ledger.Result, error) { return transition(pool, deposit, deposits.Approve, "") },
		"credit":           func() (ledger.Result, error) { return posting(pool, ledger.Credit, deposit) },
		"return to sender": func() (ledger.Result, error) { return posting(pool, ledger.ReturnToSender, deposit) },
	} {
		if r := must(run()); r != ledger.Skipped {
			t.Errorf("%s on a created deposit = %v, want skipped", name, r)
		}
	}
	if p := must(postDecision(pool, deposit)); p != ledger.PostedSkipped {
		t.Errorf("PostDecision = %v, want skipped", p)
	}
	if s := statusOf(t, pool, deposit); s != "created" {
		t.Errorf("status = %s, want created", s)
	}
	if got := balancesOf(t, pool, account); got != (balances{}) {
		t.Errorf("balances = %+v", got)
	}
	if n := len(history(t, pool, deposit)); n != 1 {
		t.Errorf("%d history rows, want 1", n)
	}
}

// Node's "rejects illegal transitions at compile time" has no runnable Go counterpart: a closed set
// of named transitions (deposits.Transition) means an illegal pair can't be written at all. What can
// be written, a converted value, is rejected at run time: see deposits' TestRejectsInvalidTransitions.

func TestNeverRemakesARecordedDecision(t *testing.T) {
	pool := testdb.Pool(t)
	_, deposit := newDeposit(t, pool)
	must(posting(pool, ledger.Book, deposit))
	must(transition(pool, deposit, deposits.StartReview, ""))
	must(transition(pool, deposit, deposits.Approve, ""))
	if r := must(transition(pool, deposit, deposits.Reject, "second opinion")); r != ledger.Skipped {
		t.Errorf("Reject after Approve = %v, want skipped", r)
	}
	if s := statusOf(t, pool, deposit); s != "approved" {
		t.Errorf("status = %s, want approved", s)
	}
}

func TestBooksExactlyOnceUnderConcurrency(t *testing.T) {
	pool := testdb.Pool(t)
	account, deposit := newDeposit(t, pool)

	const n = 10
	results := make([]ledger.Result, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			results[i], errs[i] = posting(pool, ledger.Book, deposit)
		})
	}
	close(start)
	wg.Wait()

	if err := errors.Join(errs...); err != nil { // nil if every one is nil
		t.Fatal(err)
	}
	if applied := countOf(results, ledger.Applied); applied != 1 {
		t.Errorf("%d applied, want 1 (results %v)", applied, results)
	}
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("balances = %+v", got)
	}
	if rows := len(history(t, pool, deposit)); rows != 2 {
		t.Errorf("%d history rows, want 2", rows)
	}
}

func countOf[T comparable](values []T, want T) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}
	return n
}

func TestRollsBackATransitionWhoseMovementAlreadyExists(t *testing.T) {
	pool := testdb.Pool(t)
	account, deposit := newDeposit(t, pool)
	// The ledger already holds this deposit's booking, but the deposit is still created: the two
	// guards disagree.
	must(move(pool, ledger.Movement{
		Account: account, From: ledger.BucketExternal, To: ledger.BucketPendingIn, Amount: 1250,
		Ref: ledger.DepositReference(deposit, ledger.StepBook),
	}))

	_, err := posting(pool, ledger.Book, deposit)
	if _, ok := errors.AsType[*ledger.LedgerInconsistencyError](err); !ok {
		t.Fatalf("Book error = %v, want a LedgerInconsistencyError", err)
	}
	// The status change was rolled back with it: still created, no history row added, and
	// pending_in holds only the one booking.
	if s := statusOf(t, pool, deposit); s != "created" {
		t.Errorf("status = %s, want created", s)
	}
	if n := len(history(t, pool, deposit)); n != 1 {
		t.Errorf("%d history rows, want 1", n)
	}
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("balances = %+v", got)
	}
}
