package ledger_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// inTx runs fn in its own transaction and returns what it returned: Node's
// withTransaction(pool, fn) with a result. Generic over the result type T, which Go infers from fn,
// so inTx(pool, func(tx db.Tx) (ledger.Result, error) {…}) returns a ledger.Result.
func inTx[T any](pool *pgxpool.Pool, fn func(ctx context.Context, tx db.Tx) (T, error)) (T, error) {
	ctx := context.Background()
	var result T
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		var err error
		result, err = fn(ctx, tx)
		return err
	})
	return result, err
}

// balances is an account's three buckets, compared as a whole.
type balances struct{ pendingIn, available, held money.Amount }

func balancesOf(t *testing.T, pool *pgxpool.Pool, account ids.AccountID) balances {
	t.Helper()
	b, ok, err := ledger.GetBalance(context.Background(), pool, account)
	if err != nil || !ok {
		t.Fatalf("GetBalance(%v) = %v, %v", account, ok, err)
	}
	return balances{b.PendingIn, b.Available, b.Held}
}

// movementCount counts the movements for a deposit's step.
func movementCount(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID, step ledger.DepositStep) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
SELECT count(*)::int FROM balance_movements
WHERE reference_type = 'deposit' AND reference_id = $1 AND reference_step = $2`, deposit, step).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// fundedAccount is a new account with amount in available, put there the real way, as Node's
// fundTestAccount: a deposit booked, reviewed, approved and credited.
func fundedAccount(t *testing.T, pool *pgxpool.Pool, amount money.Amount) testdb.Account {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	deposit := testdb.NewDeposit(t, pool, account, amount)
	_, err := inTx(pool, func(ctx context.Context, tx db.Tx) (struct{}, error) {
		if _, err := ledger.Book(ctx, tx, deposit); err != nil {
			return struct{}{}, err
		}
		for _, tr := range []deposits.Transition{deposits.StartReview, deposits.Approve} {
			if _, _, err := deposits.TransitionStatus(ctx, tx, deposit, tr, ""); err != nil {
				return struct{}{}, err
			}
		}
		_, err := ledger.Credit(ctx, tx, deposit)
		return struct{}{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

// testIBAN passes the withdrawals table's format CHECK (the API would have parsed it).
const testIBAN = "DE75512108001245126199"

// createWithdrawal creates a withdrawal of amount on the account, with the idempotency key (a new
// one if ""), in its own transaction. ok is false if the key was already taken.
func createWithdrawal(
	pool *pgxpool.Pool,
	account testdb.Account,
	amount money.Amount,
	key string,
) (w withdrawals.Withdrawal, ok bool, err error) {
	if key == "" {
		key = ids.New[ids.Event]().TypeID() // any unique string
	}
	err = db.InTx(context.Background(), pool, func(tx db.Tx) error {
		var err error
		w, ok, err = ledger.CreateWithdrawal(context.Background(), tx, withdrawals.NewWithdrawal{
			Account:        account.ID,
			IdempotencyKey: key,
			Amount:         amount,
			CreditorIBAN:   testIBAN,
			CreditorName:   "Jane Doe",
		})
		return err
	})
	return w, ok, err
}
