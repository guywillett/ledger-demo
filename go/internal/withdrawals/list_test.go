package withdrawals_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// The account's withdrawals list, as package deposits' ListPage: newest first, the account
// checked, the cursor and the status filter (slice 4 decisions 66 and 70). The withdrawals are
// inserted by testdb.NewWithdrawal and moved with the entity's own transitions: this package's
// tests stay below the ledger (decision 57), and a list reads statuses, not funds.

// moveTo applies the transition to the withdrawal's status.
func moveTo(t *testing.T, pool *pgxpool.Pool, id ids.WithdrawalID, transition withdrawals.Transition) {
	t.Helper()
	ctx := context.Background()
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		_, applied, err := withdrawals.TransitionStatus(ctx, tx, id, transition)
		if err == nil && !applied {
			t.Fatalf("%v didn't apply", id)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func idsOf(page withdrawals.Page) []ids.WithdrawalID {
	var found []ids.WithdrawalID
	for _, w := range page.Withdrawals {
		found = append(found, w.ID)
	}
	return found
}

// withdrawalsInEachStatus: the account's withdrawals, one per status, oldest (held) first.
func withdrawalsInEachStatus(t *testing.T, pool *pgxpool.Pool, account testdb.Account) map[withdrawals.Status]ids.WithdrawalID {
	t.Helper()
	held := testdb.NewWithdrawal(t, pool, account, 100)
	submitted := testdb.NewWithdrawal(t, pool, account, 100)
	moveTo(t, pool, submitted, withdrawals.Submit("tr_1"))
	executed := testdb.NewWithdrawal(t, pool, account, 100)
	moveTo(t, pool, executed, withdrawals.Execute(""))
	failed := testdb.NewWithdrawal(t, pool, account, 100)
	moveTo(t, pool, failed, withdrawals.Release("rejected by the beneficiary bank", ""))
	return map[withdrawals.Status]ids.WithdrawalID{
		withdrawals.StatusHeld: held, withdrawals.StatusSubmitted: submitted,
		withdrawals.StatusExecuted: executed, withdrawals.StatusFailed: failed,
	}
}

func TestListPageListsTheAccountsWithdrawalsNewestFirst(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	other := testdb.NewAccount(t, pool)
	first := testdb.NewWithdrawal(t, pool, account, 100)
	testdb.NewWithdrawal(t, pool, other, 100)
	second := testdb.NewWithdrawal(t, pool, account, 100)

	page, found, err := withdrawals.ListPage(ctx, pool, account.ID, 10, nil, withdrawals.Filter{})
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	if got := idsOf(page); !slices.Equal(got, []ids.WithdrawalID{second, first}) || page.NextBefore != nil {
		t.Errorf("got %v, next %v; want [second first], nil", got, page.NextBefore)
	}

	// An account without withdrawals: found, empty. An unknown account: not found.
	empty, found, err := withdrawals.ListPage(ctx, pool, testdb.NewAccount(t, pool).ID, 10, nil, withdrawals.Filter{})
	if err != nil || !found || len(empty.Withdrawals) != 0 {
		t.Errorf("empty account: %v, found %v, err %v", idsOf(empty), found, err)
	}
	if _, found, err := withdrawals.ListPage(ctx, pool, ids.New[ids.Account](), 10, nil, withdrawals.Filter{}); err != nil || found {
		t.Errorf("unknown account: found %v, err %v; want not found", found, err)
	}
}

func TestListPageFiltersByStatus(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	byStatus := withdrawalsInEachStatus(t, pool, account)
	for _, status := range withdrawals.Statuses() {
		page, _, err := withdrawals.ListPage(ctx, pool, account.ID, 10, nil, withdrawals.Filter{Status: &status})
		if err != nil {
			t.Fatal(err)
		}
		if got := idsOf(page); !slices.Equal(got, []ids.WithdrawalID{byStatus[status]}) {
			t.Errorf("status %s: %v, want %v", status, got, byStatus[status])
		}
	}
}

// The valid statuses as a named set, in the lifecycle's order: the status filter's 400 message
// lists them in this order, byte for byte Node's WITHDRAWAL_STATUSES.
func TestStatusesAreTheLifecycleInOrder(t *testing.T) {
	want := []withdrawals.Status{
		withdrawals.StatusHeld, withdrawals.StatusSubmitted, withdrawals.StatusExecuted, withdrawals.StatusFailed,
	}
	if got := withdrawals.Statuses(); !slices.Equal(got, want) {
		t.Errorf("Statuses() = %v, want %v", got, want)
	}
}

// limit and before apply within the filtered list, and NextBefore is the last withdrawal of the
// filtered page: following it skips the withdrawals the filter leaves out.
func TestListPagePagesThroughAFilteredList(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	var executed []ids.WithdrawalID // newest first
	for range 5 {
		id := testdb.NewWithdrawal(t, pool, account, 100)
		moveTo(t, pool, id, withdrawals.Execute(""))
		executed = slices.Insert(executed, 0, id)
		testdb.NewWithdrawal(t, pool, account, 100) // left held, between them
	}

	status := withdrawals.StatusExecuted
	var seen []ids.WithdrawalID
	var cursors []*ids.WithdrawalID
	var before *ids.WithdrawalID
	for {
		page, _, err := withdrawals.ListPage(ctx, pool, account.ID, 2, before, withdrawals.Filter{Status: &status})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, idsOf(page)...)
		cursors = append(cursors, page.NextBefore)
		if page.NextBefore == nil {
			break
		}
		before = page.NextBefore
	}
	if !slices.Equal(seen, executed) {
		t.Errorf("seen %v\nwant %v", seen, executed)
	}
	if len(cursors) != 3 || cursors[0] == nil || *cursors[0] != executed[1] ||
		cursors[1] == nil || *cursors[1] != executed[3] || cursors[2] != nil {
		t.Errorf("cursors %v, want the 2nd and 4th executed withdrawals, then nil", cursors)
	}
}
