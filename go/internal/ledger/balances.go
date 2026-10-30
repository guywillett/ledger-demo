package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
)

// Balance is an account's balances. Only Move changes them.
type Balance struct {
	PendingIn money.Amount
	Available money.Amount
	// Reserved for withdrawals in flight.
	Held money.Amount
	// A single currency: every amount is EUR minor units.
	Currency string
}

const balanceSQL = `
SELECT pending_in, available, held
FROM account_balances
WHERE account_id = $1
`

// GetBalance reads an account's balances. false if there is no such account: every account gets
// its balance row when it is created (a trigger on accounts), so a missing row means a missing
// account. Reconciliation reports any account whose row has gone missing anyway.
func GetBalance(ctx context.Context, q db.Querier, account ids.AccountID) (Balance, bool, error) {
	b := Balance{Currency: "EUR"}
	err := q.QueryRow(ctx, balanceSQL, account).Scan(&b.PendingIn, &b.Available, &b.Held)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, false, nil
	}
	if err != nil {
		return Balance{}, false, fmt.Errorf("balance of %v: %w", account, err)
	}
	return b, true, nil
}
