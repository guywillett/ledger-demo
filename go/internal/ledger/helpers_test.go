package ledger_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
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
