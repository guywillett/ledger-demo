// Package accounts is the account entity: an account read by its ID (Get). Accounts are created
// only by the seed migration and the tests (account creation is out of scope); each one's zero
// balance row comes from a trigger, and its balances are package ledger's. An entity in decision
// 57's layering: it imports only foundations (db, ids, sepa), and the API imports it. Node:
// node/src/accounts/accounts.ts.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
)

// Account is an account's own fields. Not provider_account_id: a provider's identifier for it,
// which stays at the edge (slice 4 decision 69).
type Account struct {
	ID        ids.AccountID
	Name      string
	IBAN      sepa.IBAN
	CreatedAt time.Time
}

const getSQL = `SELECT id, name, iban, created_at FROM accounts WHERE id = $1`

// Get reads an account; false if there is none.
func Get(ctx context.Context, q db.Querier, id ids.AccountID) (Account, bool, error) {
	var a Account
	var iban string
	err := q.QueryRow(ctx, getSQL, id).Scan(&a.ID, &a.Name, &iban, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, false, nil
		}
		return Account{}, false, fmt.Errorf("get account %v: %w", id, err)
	}
	// Stored only after sepa.ParseIBAN, and the column's CHECK guards the format.
	a.IBAN = sepa.IBAN(iban)
	return a, true, nil
}

const accountExistsSQL = `SELECT 1 FROM accounts WHERE id = $1`

// Exists checks an account exists.
// Node's statement; its row, if any, is read and discarded (Exec would also work, through the
// SELECT's row count, but Querier documents Exec for statements that return no rows).
func Exists(ctx context.Context, q db.Querier, id ids.AccountID) (bool, error) {
	var exists int
	err := q.QueryRow(ctx, accountExistsSQL, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("account %v exists: %w", id, err)
	}
	return true, nil
}
