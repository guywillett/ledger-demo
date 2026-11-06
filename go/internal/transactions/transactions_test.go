package transactions_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/transactions"
)

// transactions.ListPage against the test database: deposits and withdrawals interleaved, the
// cursor of either kind, the page boundary, the account scope, and the invariant violation.
// Deposits and withdrawals are inserted by testdb (rows, not funds: this reads, it doesn't post).

// idOf is an item's TypeID, from whichever of its two fields is set; it fails the test if not
// exactly one is.
func idOf(t *testing.T, item transactions.Item) string {
	t.Helper()
	switch {
	case item.Deposit != nil && item.Withdrawal == nil:
		return item.Deposit.ID.TypeID()
	case item.Withdrawal != nil && item.Deposit == nil:
		return item.Withdrawal.ID.TypeID()
	}
	t.Fatalf("item %+v: want exactly one of Deposit and Withdrawal", item)
	return ""
}

// cursorOf is a cursor's TypeID; "" for nil.
func cursorOf(t *testing.T, c *transactions.Cursor) string {
	t.Helper()
	if c == nil {
		return ""
	}
	s, ok := c.TypeID()
	if !ok {
		t.Fatalf("cursor %+v: want a deposit's or a withdrawal's ID", c)
	}
	return s
}

// interleaved: an account with a deposit, a withdrawal, a deposit and a withdrawal, in that order.
// Returns the account and their TypeIDs newest first.
func interleaved(t *testing.T, pool *pgxpool.Pool) (testdb.Account, []string) {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	var newestFirst []string
	for range 2 {
		newestFirst = slices.Insert(newestFirst, 0, testdb.NewDeposit(t, pool, account, 1250).TypeID())
		newestFirst = slices.Insert(newestFirst, 0, testdb.NewWithdrawal(t, pool, account, 100).TypeID())
	}
	return account, newestFirst
}

func page(t *testing.T, q db.Querier, account ids.AccountID, limit int, before *transactions.Cursor) transactions.Page {
	t.Helper()
	p, found, err := transactions.ListPage(context.Background(), q, account, limit, before)
	if err != nil || !found {
		t.Fatalf("ListPage = found %v, err %v", found, err)
	}
	return p
}

func idsOf(t *testing.T, p transactions.Page) []string {
	t.Helper()
	var found []string
	for _, item := range p.Items {
		found = append(found, idOf(t, item))
	}
	return found
}

func TestListPageInterleavesDepositsAndWithdrawalsNewestFirst(t *testing.T) {
	pool := testdb.Pool(t)
	account, newestFirst := interleaved(t, pool)
	p := page(t, pool, account.ID, 10, nil)
	if got := idsOf(t, p); !slices.Equal(got, newestFirst) || p.NextBefore != nil {
		t.Errorf("got %v, next %v\nwant %v, nil", got, cursorOf(t, p.NextBefore), newestFirst)
	}
	// The kinds as created: withdrawal, deposit, withdrawal, deposit.
	for i, item := range p.Items {
		if wantWithdrawal := i%2 == 0; (item.Withdrawal != nil) != wantWithdrawal {
			t.Errorf("item %d: %+v, want a withdrawal %v", i, item, wantWithdrawal)
		}
	}
}

// limit 1: every page ends on one kind and the next begins on the other, so the cursors are of
// both kinds, each followed as given.
func TestListPagePagesWithACursorOfEitherKind(t *testing.T) {
	pool := testdb.Pool(t)
	account, newestFirst := interleaved(t, pool)
	var seen, cursors []string
	var before *transactions.Cursor
	for range 10 {
		p := page(t, pool, account.ID, 1, before)
		seen = append(seen, idsOf(t, p)...)
		cursors = append(cursors, cursorOf(t, p.NextBefore))
		if p.NextBefore == nil {
			break
		}
		before = p.NextBefore
	}
	if !slices.Equal(seen, newestFirst) {
		t.Errorf("seen %v\nwant %v", seen, newestFirst)
	}
	if want := append(slices.Clone(newestFirst[:3]), ""); !slices.Equal(cursors, want) {
		t.Errorf("cursors %v, want %v", cursors, want)
	}
}

// limit + 1 detection: exactly limit items is one page with no next; one fewer leaves one over.
func TestListPageEndsExactlyAtThePageBoundary(t *testing.T) {
	pool := testdb.Pool(t)
	account, newestFirst := interleaved(t, pool)
	if p := page(t, pool, account.ID, 4, nil); len(p.Items) != 4 || p.NextBefore != nil {
		t.Errorf("limit 4: %v, next %v; want 4 items, no next", idsOf(t, p), cursorOf(t, p.NextBefore))
	}
	short := page(t, pool, account.ID, 3, nil)
	if got := cursorOf(t, short.NextBefore); got != newestFirst[2] {
		t.Errorf("limit 3: next %v, want %v", got, newestFirst[2])
	}
	if rest := page(t, pool, account.ID, 3, short.NextBefore); !slices.Equal(idsOf(t, rest), newestFirst[3:]) || rest.NextBefore != nil {
		t.Errorf("rest: %v, next %v; want %v", idsOf(t, rest), cursorOf(t, rest.NextBefore), newestFirst[3:])
	}
}

// The account in the path is a scope (decision 64): another account's items aren't listed; its
// IDs as cursors are positions (decision 66), accepted.
func TestListPageIsTheAccountsOnly(t *testing.T) {
	pool := testdb.Pool(t)
	account, newestFirst := interleaved(t, pool)
	other := testdb.NewAccount(t, pool)
	later := testdb.NewWithdrawal(t, pool, other, 100) // newer than everything of account's
	if got := idsOf(t, page(t, pool, account.ID, 10, nil)); !slices.Equal(got, newestFirst) {
		t.Errorf("got %v, want only the account's %v", got, newestFirst)
	}
	before := transactions.WithdrawalCursor(later)
	if got := idsOf(t, page(t, pool, account.ID, 10, before)); !slices.Equal(got, newestFirst) {
		t.Errorf("before another account's newer withdrawal: %v, want %v", got, newestFirst)
	}
}

func TestListPageEmptyAndUnknownAccounts(t *testing.T) {
	pool := testdb.Pool(t)
	empty := page(t, pool, testdb.NewAccount(t, pool).ID, 10, nil)
	if len(empty.Items) != 0 || empty.NextBefore != nil {
		t.Errorf("empty account: %v", idsOf(t, empty))
	}
	if _, found, err := transactions.ListPage(context.Background(), pool, ids.New[ids.Account](), 10, nil); err != nil || found {
		t.Errorf("unknown account: found %v, err %v; want not found", found, err)
	}
}

// droppingByID passes every statement to the pool but the reads by ID (deposits.GetMany and
// withdrawals.GetMany), which it makes find nothing: the page is decided on rows that the read by
// ID then doesn't return, the case the real database can't produce (rows are never deleted, an
// account never changes).
type droppingByID struct{ pool *pgxpool.Pool }

func (d droppingByID) rewrite(sql string) string {
	return strings.Replace(sql, "AND id = ANY(", "AND false AND id = ANY(", 1)
}

func (d droppingByID) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return d.pool.Exec(ctx, d.rewrite(sql), args...)
}

func (d droppingByID) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return d.pool.Query(ctx, d.rewrite(sql), args...)
}

func (d droppingByID) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return d.pool.QueryRow(ctx, d.rewrite(sql), args...)
}

// A row missing from the read by ID is an invariant violation (decision 67): an error naming the
// missing IDs, never a shorter page.
func TestListPageReportsARowMissingFromTheReadByIDAsAnError(t *testing.T) {
	pool := testdb.Pool(t)
	account, newestFirst := interleaved(t, pool)
	p, _, err := transactions.ListPage(context.Background(), droppingByID{pool}, account.ID, 10, nil)
	if err == nil {
		t.Fatalf("no error, page %v; want the missing rows reported", p.Items)
	}
	for _, id := range newestFirst {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error %q doesn't name %s", err, id)
		}
	}
	if len(p.Items) != 0 {
		t.Errorf("a page %v with the error; want none", p.Items)
	}
}
