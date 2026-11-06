// Package transactions is an account's deposits and withdrawals together, newest first (slice 4
// decision 67): a read model over the two entities, so it sits above both (decision 57's
// orchestration layer, beside ingestion and compliance). Node: node/src/transactions/.
//
// How one ordered page comes from two tables:
//  1. one UNION ALL of (type, id), each branch its own account-scoped, index-ordered query with the
//     cursor and limit + 1, decides the page: which items, in what order, and whether there is a
//     next one, on IDs alone, in one statement;
//  2. each kind's full rows are then read by ID within the account, one statement per kind on the
//     page (deposits.GetMany, withdrawals.GetMany), with the same scans as their own endpoints, and
//     put in the order step 1 decided.
//
// At most four statements with the account check, whatever the page size: no N+1. No transaction
// around them (READ COMMITTED): the page is decided on IDs alone, deposits and withdrawals are
// never deleted, and their account never changes, so step 2 finds every ID step 1 chose; only an
// item's state can be newer than step 1's snapshot, which is what its own GET would show at that
// moment. That stops holding if an account can change: e.g. resolving a deposit from suspense into
// an account (out of scope). A row step 2 can't find is therefore an invariant violation, an
// error (the API's 500), never a shorter page.
package transactions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/accounts"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// Item is one transaction: a deposit or a withdrawal, exactly one of the two set, as its own
// package reads it.
type Item struct {
	Deposit    *deposits.Deposit
	Withdrawal *withdrawals.Withdrawal
}

// Cursor is an item's position in the list: a deposit's or a withdrawal's ID. Made by DepositCursor
// or WithdrawalCursor, so exactly one of the two is set (the fields are unexported for that; Go
// can't forbid the zero value, which has neither, so every method handles it). As a list's before,
// only the ID's UUID counts: UUIDv7 IDs sort by creation time across both tables, so either kind's
// ID is a position in time (decision 66).
type Cursor struct {
	deposit    *ids.DepositID
	withdrawal *ids.WithdrawalID
}

// DepositCursor is a deposit's position.
func DepositCursor(id ids.DepositID) *Cursor { return &Cursor{deposit: &id} }

// WithdrawalCursor is a withdrawal's position.
func WithdrawalCursor(id ids.WithdrawalID) *Cursor { return &Cursor{withdrawal: &id} }

// uuid is the position's UUID, the value the page's query compares; false for a nil or empty
// cursor (no position: the first page).
func (c *Cursor) uuid() (uuid.UUID, bool) {
	switch {
	case c == nil:
		return uuid.Nil, false
	case c.deposit != nil:
		return c.deposit.UUID(), true
	case c.withdrawal != nil:
		return c.withdrawal.UUID(), true
	default:
		return uuid.Nil, false
	}
}

// TypeID is the position's TypeID, dep_… or wd_… by its kind, as the API writes next_before;
// false for a nil or empty cursor.
func (c *Cursor) TypeID() (string, bool) {
	switch {
	case c == nil:
		return "", false
	case c.deposit != nil:
		return c.deposit.TypeID(), true
	case c.withdrawal != nil:
		return c.withdrawal.TypeID(), true
	default:
		return "", false
	}
}

// MarshalText writes the position as its TypeID, dep_… or wd_… (encoding.TextMarshaler), so
// encoding/json writes a Cursor as a JSON string, as it does an ids.ID, and a nil *Cursor as null.
// An empty cursor (neither ID set) is an error: it can't be written, and nothing makes one.
// A value receiver, so encoding/json finds it on Cursor and *Cursor alike;
// a nil *Cursor it writes as null without calling it.
func (c Cursor) MarshalText() ([]byte, error) {
	s, ok := c.TypeID()
	if !ok {
		return nil, errors.New("transactions: an empty cursor has no TypeID")
	}
	return []byte(s), nil
}

// Page is a page of an account's transactions, newest first.
type Page struct {
	Items      []Item
	NextBefore *Cursor // pass as before for the next (older) page; nil on the last page
}

// listSQL is the page's statement (step 1 above): one UNION ALL of each table's account-scoped,
// index-ordered branch, each with its own ORDER BY and LIMIT $1 (limit + 1), so it stops after
// that many rows of its (account_id, id DESC) index whatever the account's history; no more than
// that many of the merged page can come from one table. The conditions (the account, and the
// cursor if any) are the same in both branches. type breaks a tie between equal IDs, so the order
// is total. Built from fixed fragments, the values always parameters.
func listSQL(where string) string {
	return `
SELECT type, id FROM (
  (SELECT 'deposit' AS type, id FROM deposits
   WHERE ` + where + `
   ORDER BY id DESC
   LIMIT $1)
  UNION ALL
  (SELECT 'withdrawal' AS type, id FROM withdrawals
   WHERE ` + where + `
   ORDER BY id DESC
   LIMIT $1)
) AS page
ORDER BY id DESC, type
LIMIT $1
`
}

// scanCursor reads one row of listSQL, (type, id), as the item's Cursor.
func scanCursor(row pgx.Row) (Cursor, error) {
	var kind string
	var id ids.DepositID // either kind's ID scans the same way: the column is a uuid
	if err := row.Scan(&kind, &id); err != nil {
		return Cursor{}, err
	}
	if kind == "withdrawal" {
		// The explicit conversion between kinds that package ids allows (see ids.ID): the row's type
		// says which kind of ID it is.
		w := ids.WithdrawalID(id)
		return Cursor{withdrawal: &w}, nil
	}
	return Cursor{deposit: &id}, nil
}

// ListPage reads a page of at most limit of the account's transactions, newest first, older than
// before if it isn't nil, as the package comment describes. found is false for an unknown account;
// an account without transactions gets an empty page. On an error, the other results are zero and
// meaningless (Go's convention: the caller checks the error first). A row the page was decided on
// but its read by ID doesn't return is such an error, naming the missing IDs.
func ListPage(
	ctx context.Context,
	q db.Querier,
	account ids.AccountID,
	limit int,
	before *Cursor,
) (Page, bool, error) {
	exists, err := accounts.Exists(ctx, q, account)
	if err != nil {
		return Page{}, false, err
	}
	if !exists {
		return Page{}, false, nil
	}
	// Step 1: the page, decided on IDs. listSQL's conditions, the same in both branches. Limit is one
	// more than the page size, to know whether there is a next page.
	where := db.NewConditions(limit + 1) // $1
	where.Where("account_id", "=", account)
	if v, ok := before.uuid(); ok {
		// Seek pagination on the item's ID: UUIDv7 IDs sort by creation time across both tables, so
		// "older than the last one seen" is id < $before in each branch, whichever kind it was.
		where.Where("id", "<", v)
	}

	rows, err := q.Query(ctx, listSQL(where.SQL()), where.Params()...)
	if err != nil {
		return Page{}, false, fmt.Errorf("transactions of %v: %w", account, err)
	}
	// pgx.CollectRows reads every row with the function given and closes rows, returning the first
	// error: the loop of rows.Next / Scan / rows.Err, written once by pgx.
	cursors, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Cursor, error) {
		return scanCursor(row)
	})
	if err != nil {
		return Page{}, false, fmt.Errorf("transactions of %v: %w", account, err)
	}
	if len(cursors) == 0 {
		return Page{}, true, nil // the account has no transactions
	}

	hasMore := len(cursors) > limit
	if hasMore {
		cursors = cursors[:limit]
	}

	// Step 2: each kind's full rows, read by ID within the account (only the page's: the extra row
	// was only to know whether there is a next page).
	var depTxIDs []ids.DepositID
	var wdTxIDs []ids.WithdrawalID
	for _, cursor := range cursors {
		if cursor.deposit != nil {
			depTxIDs = append(depTxIDs, *cursor.deposit)
		} else {
			wdTxIDs = append(wdTxIDs, *cursor.withdrawal)
		}
	}

	depRows, err := deposits.GetMany(ctx, q, account, depTxIDs)
	if err != nil {
		return Page{}, false, fmt.Errorf("transactions (deposits) of %v: %w", account, err)
	}
	wdRows, err := withdrawals.GetMany(ctx, q, account, wdTxIDs)
	if err != nil {
		return Page{}, false, fmt.Errorf("transactions (withdrawals) of %v: %w", account, err)
	}
	// By ID, to put the rows in the order step 1 decided.
	depMap := make(map[ids.DepositID]deposits.Deposit)
	for _, r := range depRows {
		depMap[r.ID] = r
	}
	wdMap := make(map[ids.WithdrawalID]withdrawals.Withdrawal)
	for _, r := range wdRows {
		wdMap[r.ID] = r
	}

	var items []Item
	var missing []string
	for _, cursor := range cursors {
		if cursor.deposit != nil {
			if deposit, ok := depMap[*cursor.deposit]; ok {
				items = append(items, Item{Deposit: &deposit})
			} else {
				missing = append(missing, cursor.deposit.TypeID())
			}
		} else {
			if withdrawal, ok := wdMap[*cursor.withdrawal]; ok {
				items = append(items, Item{Withdrawal: &withdrawal})
			} else {
				missing = append(missing, cursor.withdrawal.TypeID())
			}
		}
	}

	if len(missing) > 0 {
		return Page{}, false, fmt.Errorf(
			"transactions of %v: decided on, then not found by ID (an invariant violation): %s",
			account, strings.Join(missing, ", "))
	}

	page := Page{Items: items}
	if hasMore {
		last := cursors[limit-1]
		page.NextBefore = &last
	}

	return page, true, nil
}
