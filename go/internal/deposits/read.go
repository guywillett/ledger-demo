package deposits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/accounts"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
)

// Deposit is a stored deposit matched to an account (any status but unmatched). Node:
// node/src/deposits/deposits.ts. A deposit in suspense has no account and its own type in Node;
// Go never reads one back (ingestion only writes them, and reconciliation, which lists them, is
// Node's), so it has none. Nullable columns are pointers, as in withdrawals.
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

// Filter holds values that ListPage should filter results by. A zero value does not filter.
type Filter struct {
	Status               *Status
	NetworkTransactionID *string
}

// Page is a page of an account's deposits, newest first.
type Page struct {
	Deposits []Deposit
	// Pass as before to get the next (older) page; nil on the last page.
	NextBefore *ids.DepositID
}

// listSQL is the list's statement, its conditions (a db.Conditions' SQL: the account, the cursor
// and the filters present) after WHERE, LIMIT $1 one more than the page size.
func listSQL(where string) string {
	return `
SELECT ` + columns + ` FROM deposits
WHERE ` + where + `
ORDER BY id DESC
LIMIT $1
`
}

const getSQL = `SELECT ` + columns + ` FROM deposits WHERE id = $1 AND account_id = $2`

const getManySQL = `SELECT ` + columns + ` FROM deposits WHERE account_id = $1 AND id = ANY($2)`

// GetMany reads the account's deposits among list, in one statement, in no particular order; an ID
// the account has no deposit for is simply absent, and the caller decides what that means. For the
// transactions list, which decides its page on IDs alone and then reads each kind's rows by ID
// (slice 4 decision 67). Node: getDeposits.
func GetMany(
	ctx context.Context,
	q db.Querier,
	account ids.AccountID,
	list []ids.DepositID,
) ([]Deposit, error) {
	if len(list) == 0 {
		return nil, nil
	}
	// As text, as StatusHistories passes them: Postgres reads the elements as the column's uuid.
	uuids := make([]string, len(list))
	for i, id := range list {
		uuids[i] = id.UUID().String()
	}
	rows, err := q.Query(ctx, getManySQL, account, uuids)
	if err != nil {
		return nil, fmt.Errorf("deposits of %v by ID: %w", account, err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Deposit, error) {
		return scanDeposit(row)
	})
	if err != nil {
		return nil, fmt.Errorf("deposits of %v by ID: %w", account, err)
	}
	return found, nil
}

// Get reads a deposit of the account; false if the account has no such deposit, whether there is
// none or it is another account's (slice 4 decision 64: one statement decides). An unmatched
// deposit has no account, so it is never found.
func Get(
	ctx context.Context,
	q db.Querier,
	account ids.AccountID,
	id ids.DepositID,
) (Deposit, bool, error) {
	d, err := scanDeposit(q.QueryRow(ctx, getSQL, id, account))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deposit{}, false, nil
	}
	if err != nil {
		return Deposit{}, false, fmt.Errorf("deposit %v of %v: %w", id, account, err)
	}
	return d, true, nil
}

// ListPage reads a page of at most limit deposits of the account, newest first, older than before
// if it isn't nil. false if there is no such account (an account without deposits gets an empty
// page). The existence check is a read before a read: nothing is written based on it.
func ListPage(
	ctx context.Context,
	q db.Querier,
	account ids.AccountID,
	limit int,
	before *ids.DepositID,
	filter Filter,
) (Page, bool, error) {
	exists, err := accounts.Exists(ctx, q, account)
	if err != nil {
		return Page{}, false, err
	}
	if !exists {
		return Page{}, false, nil
	}
	// listSQL's conditions. Limit is one more than the page size, to know whether there is a next
	// page.
	where := db.NewConditions(limit + 1) // $1
	where.Where("account_id", "=", account)
	if before != nil {
		// Seek pagination on the deposit ID: UUIDv7 IDs sort by creation time, so "older than the
		// last one seen" is id < $before, served by the (account_id, id DESC) index.
		where.Where("id", "<", *before)
	}
	if filter.Status != nil {
		where.Where("status", "=", *filter.Status)
	}
	if filter.NetworkTransactionID != nil {
		where.Where("network_transaction_id", "=", *filter.NetworkTransactionID)
	}

	rows, err := q.Query(ctx, listSQL(where.SQL()), where.Params()...)
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

const existsSQL = `SELECT EXISTS (SELECT 1 FROM deposits WHERE id = $1)`

// Exists reports whether a deposit exists. For classifying an outcome after the fact (an event
// whose transitions all skipped: already processed, or a deposit that doesn't exist), never for
// deciding whether to act. Node: depositExists. SELECT EXISTS always returns one row, true or
// false, so there is no "no rows" case to tell apart from an error.
func Exists(ctx context.Context, q db.Querier, id ids.DepositID) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, existsSQL, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("deposit %v exists: %w", id, err)
	}
	return exists, nil
}
