package ledger_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// Move against the test database: the cases of node/test/ledger/move.test.ts. Each test uses its
// own account, so balances start at zero. The two cases that move money through held use a
// withdrawal reference and come with the withdrawal postings (step 3b).

// booking is a deposit's book movement for the account, with a fresh deposit ID.
func booking(account ids.AccountID) ledger.Movement {
	return ledger.Movement{
		Account: account,
		From:    ledger.BucketExternal,
		To:      ledger.BucketPendingIn,
		Amount:  1250,
		Ref:     ledger.DepositReference(ids.New[ids.Deposit](), ledger.StepBook),
	}
}

func move(pool *pgxpool.Pool, m ledger.Movement) (ledger.MoveResult, error) {
	return inTx(pool, func(ctx context.Context, tx db.Tx) (ledger.MoveResult, error) {
		return ledger.Move(ctx, tx, m)
	})
}

func TestMovesBetweenTheRightBuckets(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	deposit := ids.New[ids.Deposit]()
	book := booking(account)
	book.Ref = ledger.DepositReference(deposit, ledger.StepBook)
	if r, err := move(pool, book); r != ledger.MoveApplied || err != nil {
		t.Fatalf("book = %v, %v; want applied", r, err)
	}
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("after booking: %+v", got)
	}

	credit := book
	credit.From, credit.To = ledger.BucketPendingIn, ledger.BucketAvailable
	credit.Ref = ledger.DepositReference(deposit, ledger.StepCredit)
	if r, err := move(pool, credit); r != ledger.MoveApplied || err != nil {
		t.Fatalf("credit = %v, %v; want applied", r, err)
	}
	if got := balancesOf(t, pool, account); got != (balances{0, 1250, 0}) {
		t.Errorf("after crediting: %+v", got)
	}
}

func TestMovesAmountsBeyond2To53Exactly(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	m := booking(account)
	m.Amount = 9007199254740993 // 2^53 + 1: the smallest integer a float64 can't hold
	if _, err := move(pool, m); err != nil {
		t.Fatal(err)
	}
	if got := balancesOf(t, pool, account); got != (balances{9007199254740993, 0, 0}) {
		t.Errorf("balances = %+v", got)
	}
}

func TestIsANoOpWhenCalledTwiceWithTheSameReference(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	deposit := ids.New[ids.Deposit]()
	m := booking(account)
	m.Ref = ledger.DepositReference(deposit, ledger.StepBook)
	for i, want := range []ledger.MoveResult{ledger.MoveApplied, ledger.MoveDuplicate, ledger.MoveDuplicate} {
		if r, err := move(pool, m); r != want || err != nil {
			t.Fatalf("call %d = %v, %v; want %v", i+1, r, err, want)
		}
	}
	// Also twice within one transaction.
	r, err := inTx(pool, func(ctx context.Context, tx db.Tx) (ledger.MoveResult, error) {
		if _, err := ledger.Move(ctx, tx, m); err != nil {
			return "", err
		}
		return ledger.Move(ctx, tx, m)
	})
	if r != ledger.MoveDuplicate || err != nil {
		t.Fatalf("twice in one transaction = %v, %v; want duplicate", r, err)
	}
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("balances = %+v", got)
	}
	if n := movementCount(t, pool, deposit, ledger.StepBook); n != 1 {
		t.Errorf("%d movements, want 1", n)
	}
}

func TestRefusesAReusedReferenceWithDifferentDetails(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	other := testdb.NewAccount(t, pool).ID
	m := booking(account)
	if _, err := move(pool, m); err != nil {
		t.Fatal(err)
	}
	amount, otherAccount, otherBucket := m, m, m
	amount.Amount = 999
	otherAccount.Account = other
	otherBucket.To = ledger.BucketAvailable
	for name, reused := range map[string]ledger.Movement{
		"another amount": amount, "another account": otherAccount, "another bucket": otherBucket,
	} {
		_, err := move(pool, reused)
		// errors.AsType returns the error as its concrete type if it is one (anywhere in the chain
		// of wrapped errors), so its fields can be read.
		if _, ok := errors.AsType[*ledger.MovementConflictError](err); !ok {
			t.Errorf("%s: error = %v, want a MovementConflictError", name, err)
		}
	}
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("account balances = %+v", got)
	}
	if got := balancesOf(t, pool, other); got != (balances{}) {
		t.Errorf("other account balances = %+v", got)
	}
}

// Ten goroutines (Go's lightweight threads: `go f()` starts f running concurrently) each move the
// same reference in their own transaction, all released at once. Exactly one applies.
func TestAppliesExactlyOnceWhenCalledConcurrently(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	deposit := ids.New[ids.Deposit]()
	m := booking(account)
	m.Ref = ledger.DepositReference(deposit, ledger.StepBook)

	const n = 10
	results := make([]ledger.MoveResult, n)
	errs := make([]error, n)
	start := make(chan struct{}) // closed to release every goroutine together
	var wg sync.WaitGroup
	for i := range n {
		// wg.Go runs the function in a new goroutine and counts it; wg.Wait below returns once all
		// have finished. Each goroutine writes only its own slot of results and errs, so they share
		// no memory and need no lock (the race detector, go test -race, would report it otherwise).
		wg.Go(func() {
			<-start // a receive from a channel blocks until a value is sent, or the channel closes
			results[i], errs[i] = move(pool, m)
		})
	}
	close(start)
	wg.Wait()

	applied, duplicate := 0, 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		switch results[i] {
		case ledger.MoveApplied:
			applied++
		case ledger.MoveDuplicate:
			duplicate++
		}
	}
	if applied != 1 || duplicate != n-1 {
		t.Errorf("applied %d, duplicate %d; want 1 and %d", applied, duplicate, n-1)
	}
	if got := balancesOf(t, pool, account); got != (balances{1250, 0, 0}) {
		t.Errorf("balances = %+v", got)
	}
	if c := movementCount(t, pool, deposit, ledger.StepBook); c != 1 {
		t.Errorf("%d movements, want 1", c)
	}
}

func TestCannotOverdrawABucket(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	deposit := ids.New[ids.Deposit]()
	m := booking(account)
	m.From, m.To = ledger.BucketPendingIn, ledger.BucketAvailable
	m.Ref = ledger.DepositReference(deposit, ledger.StepCredit)

	_, err := move(pool, m)
	overdraft, ok := errors.AsType[*ledger.OverdraftError](err)
	if !ok {
		t.Fatalf("error = %v, want an OverdraftError", err)
	}
	if overdraft.Bucket != ledger.BucketPendingIn || overdraft.Account != account || overdraft.Amount != 1250 {
		t.Errorf("OverdraftError = %+v", overdraft)
	}
	// The database's check_violation is kept, and errors.AsType finds it through Unwrap.
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "23514" {
		t.Errorf("wrapped error = %v, want a check_violation (23514)", err)
	}
	if got := balancesOf(t, pool, account); got != (balances{}) {
		t.Errorf("balances = %+v", got)
	}
	if c := movementCount(t, pool, deposit, ledger.StepCredit); c != 0 {
		t.Errorf("%d movements, want 0: the transaction rolled back", c)
	}
}

func TestMoveOnceReportsADuplicateAsLedgerInconsistency(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	deposit := ids.New[ids.Deposit]()
	m := booking(account)
	m.Ref = ledger.DepositReference(deposit, ledger.StepBook)
	moveOnce := func() error {
		return db.InTx(context.Background(), pool, func(tx db.Tx) error {
			return ledger.MoveOnce(context.Background(), tx, m)
		})
	}
	if err := moveOnce(); err != nil {
		t.Fatal(err)
	}

	err := moveOnce()
	if _, ok := errors.AsType[*ledger.LedgerInconsistencyError](err); !ok {
		t.Fatalf("error = %v, want a LedgerInconsistencyError", err)
	}
	want := "deposit " + deposit.TypeID() + ": its book movement already exists, but the deposit's state says that step hasn't happened yet"
	if err.Error() != want {
		t.Errorf("message = %q\nwant      %q", err, want)
	}
	if c := movementCount(t, pool, deposit, ledger.StepBook); c != 1 {
		t.Errorf("%d movements, want 1", c)
	}
	if got := balancesOf(t, pool, account); got.pendingIn != 1250 {
		t.Errorf("pending_in = %v, want 1250", got.pendingIn)
	}
}

func TestFailsWithoutABalanceRow(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	// The trigger creates the row with the account, so delete it to get here.
	if _, err := pool.Exec(context.Background(), `DELETE FROM account_balances WHERE account_id = $1`, account); err != nil {
		t.Fatal(err)
	}
	deposit := ids.New[ids.Deposit]()
	m := booking(account)
	m.Ref = ledger.DepositReference(deposit, ledger.StepBook)
	if _, err := move(pool, m); err == nil || !strings.Contains(err.Error(), "no balance row") {
		t.Errorf("error = %v, want no balance row", err)
	}
	if c := movementCount(t, pool, deposit, ledger.StepBook); c != 0 {
		t.Errorf("%d movements, want 0", c)
	}
}

func TestRejectsInvalidMovements(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool).ID
	sameBucket, zero, negative, noReference := booking(account), booking(account), booking(account), booking(account)
	sameBucket.To = ledger.BucketExternal
	zero.Amount = 0
	negative.Amount = -1
	noReference.Ref = ledger.Reference{}
	tests := []struct {
		name string
		m    ledger.Movement
		want string
	}{
		{"to the same bucket", sameBucket, "to itself"},
		{"a zero amount", zero, "positive"},
		{"a negative amount", negative, "positive"},
		{"no reference", noReference, "without a reference"},
	}
	for _, tt := range tests {
		if _, err := move(pool, tt.m); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestKeepsBalanceMovementsAppendOnly(t *testing.T) {
	pool := testdb.Pool(t)
	m := booking(testdb.NewAccount(t, pool).ID)
	deposit := ids.New[ids.Deposit]()
	m.Ref = ledger.DepositReference(deposit, ledger.StepBook)
	if _, err := move(pool, m); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for statement, want := range map[string]string{
		`UPDATE balance_movements SET amount_minor = 1 WHERE reference_id = $1`: "append-only: UPDATE",
		`DELETE FROM balance_movements WHERE reference_id = $1`:                 "append-only: DELETE",
	} {
		if _, err := pool.Exec(ctx, statement, deposit); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", statement, err, want)
		}
	}
}

func TestIsInvariantViolation(t *testing.T) {
	wrap := func(err error) error { return errors.Join(errors.New("context"), err) }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"LedgerInconsistencyError", &ledger.LedgerInconsistencyError{}, true},
		{"MovementConflictError", &ledger.MovementConflictError{}, true},
		{"OverdraftError", &ledger.OverdraftError{}, true},
		{"any check_violation", &pgconn.PgError{Code: "23514"}, true},
		{"a wrapped violation", wrap(&ledger.MovementConflictError{}), true},
		{"another database error", &pgconn.PgError{Code: "40001"}, false},
		{"a plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		if got := ledger.IsInvariantViolation(tt.err); got != tt.want {
			t.Errorf("%s: IsInvariantViolation = %v, want %v", tt.name, got, tt.want)
		}
	}
}
