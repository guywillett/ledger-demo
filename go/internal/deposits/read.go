package deposits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
)

// Deposit is a stored deposit matched to an account (any status but unmatched). Node:
// node/src/deposits/deposits.ts. A deposit in suspense has no account and its own type in Node;
// Go gets it with ingestion (step 4b). Nullable columns are pointers, as in withdrawals.
type Deposit struct {
	ID                   ids.DepositID
	Account              ids.AccountID
	CreditorIBAN         sepa.IBAN
	ProviderAccountID    *string
	Amount               money.Amount
	Currency             string
	Network              string
	NetworkTransactionID string
	Status               Status
	NotificationCount    int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// columns lists a deposit's columns in the order scanDeposit reads them (explicit rather than
// Node's SELECT *: Go scans by position).
const columns = `id, account_id, creditor_iban, provider_account_id, amount_minor, currency,
network, network_transaction_id, status, notification_count, created_at, updated_at`

// CheckCurrencyAndNetwork rejects a stored deposit row with a currency or network this code doesn't
// handle (the columns are text; only EUR and SEPA are written): every mapping of a deposits row
// checks it, as Node's depositFromRow and unmatchedDepositFromRow do (expectLiteral).
func CheckCurrencyAndNetwork(id ids.DepositID, currency, network string) error {
	if currency != "EUR" || network != "SEPA" {
		return fmt.Errorf("deposit %v: unexpected currency or network in database: %q, %q",
			id, currency, network)
	}
	return nil
}

func scanDeposit(row pgx.Row) (Deposit, error) {
	var d Deposit
	var iban, status string
	err := row.Scan(&d.ID, &d.Account, &iban, &d.ProviderAccountID, &d.Amount, &d.Currency,
		&d.Network, &d.NetworkTransactionID, &status, &d.NotificationCount, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return Deposit{}, err
	}
	if err := CheckCurrencyAndNetwork(d.ID, d.Currency, d.Network); err != nil {
		return Deposit{}, err
	}
	// Stored only after sepa.ParseIBAN, and the column's CHECK guards the format.
	d.CreditorIBAN = sepa.IBAN(iban)
	if d.Status, err = ParseStatus(status); err != nil {
		return Deposit{}, fmt.Errorf("deposit %v: %w", d.ID, err)
	}
	if d.Status == StatusUnmatched {
		return Deposit{}, fmt.Errorf("deposit %v is unmatched, not a booked deposit", d.ID)
	}
	return d, nil
}

// Page is a page of an account's deposits, newest first.
type Page struct {
	Deposits []Deposit
	// Pass as before to get the next (older) page; nil on the last page.
	NextBefore *ids.DepositID
}

const accountExistsSQL = `SELECT 1 FROM accounts WHERE id = $1`

// Seek pagination on the deposit ID: UUIDv7 IDs sort by creation time, so "older than the last one
// seen" is id < $before, served by the (account_id, id DESC) index. Two fixed statements rather
// than one with an optional condition, which keeps each one simple to plan.
const firstPageSQL = `
SELECT ` + columns + ` FROM deposits
WHERE account_id = $1
ORDER BY id DESC
LIMIT $2
`

const nextPageSQL = `
SELECT ` + columns + ` FROM deposits
WHERE account_id = $1 AND id < $3
ORDER BY id DESC
LIMIT $2
`

// ListPage reads a page of at most limit deposits of the account, newest first, older than before
// if it isn't nil. false if there is no such account (an account without deposits gets an empty
// page). The existence check is a read before a read: nothing is written based on it.
func ListPage(
	ctx context.Context,
	q db.Querier,
	account ids.AccountID,
	limit int,
	before *ids.DepositID,
) (Page, bool, error) {
	// Node's statement; its row, if any, is read and discarded (Exec would also work, through the
	// SELECT's row count, but Querier documents Exec for statements that return no rows).
	var one int
	err := q.QueryRow(ctx, accountExistsSQL, account).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return Page{}, false, nil
	}
	if err != nil {
		return Page{}, false, fmt.Errorf("account %v: %w", account, err)
	}

	// One more than the page size, to know whether there is a next page.
	var rows pgx.Rows
	if before == nil {
		rows, err = q.Query(ctx, firstPageSQL, account, limit+1)
	} else {
		rows, err = q.Query(ctx, nextPageSQL, account, limit+1, *before)
	}
	if err != nil {
		return Page{}, false, fmt.Errorf("deposits of %v: %w", account, err)
	}
	// pgx.CollectRows reads every row with the function given and closes rows, returning the first
	// error: the loop of rows.Next / Scan / rows.Err, written once by pgx.
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Deposit, error) {
		return scanDeposit(row)
	})
	if err != nil {
		return Page{}, false, fmt.Errorf("deposits of %v: %w", account, err)
	}

	page := Page{Deposits: found}
	if len(found) > limit {
		page.Deposits = found[:limit]
		last := page.Deposits[limit-1].ID
		page.NextBefore = &last
	}
	return page, true, nil
}

// StatusChange is one row of a deposit's status history. From is nil for the first row, Reason nil
// unless the change recorded one.
type StatusChange struct {
	From   *Status
	To     Status
	Reason *string
	At     time.Time
}

const historiesSQL = `
SELECT deposit_id, from_status, to_status, reason, at
FROM deposit_status_history
WHERE deposit_id = ANY($1)
ORDER BY deposit_id, id
`

// StatusHistories reads the status history of several deposits in one query (not one query per
// deposit), oldest change first. Deposits without history rows are absent from the map.
func StatusHistories(
	ctx context.Context,
	q db.Querier,
	deposits []ids.DepositID,
) (map[ids.DepositID][]StatusChange, error) {
	histories := make(map[ids.DepositID][]StatusChange)
	if len(deposits) == 0 {
		return histories, nil
	}
	// As text: pgx encodes a []string for ANY($1), and Postgres reads the elements as the column's
	// uuid (the same parameter as Node's).
	uuids := make([]string, len(deposits))
	for i, id := range deposits {
		uuids[i] = id.UUID().String()
	}
	rows, err := q.Query(ctx, historiesSQL, uuids)
	if err != nil {
		return nil, fmt.Errorf("deposit histories: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id ids.DepositID
		var change StatusChange
		var from *string
		var to string
		if err := rows.Scan(&id, &from, &to, &change.Reason, &change.At); err != nil {
			return nil, err
		}
		if from != nil {
			status, err := ParseStatus(*from)
			if err != nil {
				return nil, err
			}
			change.From = &status
		}
		if change.To, err = ParseStatus(to); err != nil {
			return nil, err
		}
		histories[id] = append(histories[id], change)
	}
	return histories, rows.Err()
}
