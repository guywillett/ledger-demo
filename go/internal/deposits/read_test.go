package deposits_test

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// deposits.Get: one deposit, read within its account (slice 4 decision 64): found, and not found
// both when it doesn't exist and when it belongs to another account.

func TestGetReadsADepositWithinItsAccount(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool)
	deposit := testdb.NewDeposit(t, pool, account, 1250)

	got, found, err := deposits.Get(context.Background(), pool, account.ID, deposit)
	if err != nil || !found {
		t.Fatalf("Get = found %v, err %v; want the deposit", found, err)
	}
	// The same deposit as the list reads.
	page, _, err := deposits.ListPage(context.Background(), pool, account.ID, 1, nil, deposits.Filter{})
	if err != nil || len(page.Deposits) != 1 {
		t.Fatalf("ListPage = %v, %v", page, err)
	}
	if !reflect.DeepEqual(got, page.Deposits[0]) {
		t.Errorf("Get = %+v\nwant  %+v (as ListPage reads it)", got, page.Deposits[0])
	}
	if got.ID != deposit || got.Account != account.ID || got.Status != deposits.StatusCreated {
		t.Errorf("Get = %+v", got)
	}
}

func TestGetDoesNotFindAnotherAccountsDeposit(t *testing.T) {
	pool := testdb.Pool(t)
	owner := testdb.NewAccount(t, pool)
	other := testdb.NewAccount(t, pool)
	deposit := testdb.NewDeposit(t, pool, owner, 1250)

	_, found, err := deposits.Get(context.Background(), pool, other.ID, deposit)
	if err != nil || found {
		t.Errorf("Get under another account = found %v, err %v; want not found, no error", found, err)
	}
}

func TestGetReportsAnUnknownDepositAsNotFound(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool)

	_, found, err := deposits.Get(context.Background(), pool, account.ID, ids.New[ids.Deposit]())
	if err != nil || found {
		t.Errorf("Get(unknown) = found %v, err %v; want not found, no error", found, err)
	}
}

// moveTo applies the transitions to the deposit's status, in order, without the ledger's money
// (this package's tests stay below the ledger: decision 57). Enough to give a deposit a status.
func moveTo(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID, steps ...deposits.Transition) {
	t.Helper()
	ctx := context.Background()
	for _, step := range steps {
		err := db.InTx(ctx, pool, func(tx db.Tx) error {
			_, applied, err := deposits.TransitionStatus(ctx, tx, deposit, step, "")
			if err == nil && !applied {
				t.Fatalf("%v didn't apply to %v", step, deposit)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func idsOf(page deposits.Page) []ids.DepositID {
	var found []ids.DepositID
	for _, d := range page.Deposits {
		found = append(found, d.ID)
	}
	return found
}

func ptr[T any](v T) *T { return &v }

// ListPage's filters (slice 4 decision 70): status and network_transaction_id, alone and together,
// nil meaning no filter (Filter's zero value filters nothing).
func TestListPageFilters(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	created := testdb.NewDeposit(t, pool, account, 1250)
	received := testdb.NewDeposit(t, pool, account, 1250)
	moveTo(t, pool, received, deposits.Book)
	// testdb.NewDeposit stores network_transaction_id "go-test-" and the deposit's TypeID.
	receivedNTX := "go-test-" + received.TypeID()

	tests := []struct {
		name   string
		filter deposits.Filter
		want   []ids.DepositID
	}{
		{"none", deposits.Filter{}, []ids.DepositID{received, created}},
		{"status", deposits.Filter{Status: ptr(deposits.StatusCreated)}, []ids.DepositID{created}},
		{"a valid status none is in", deposits.Filter{Status: ptr(deposits.StatusCredited)}, nil},
		{"network_transaction_id", deposits.Filter{NetworkTransactionID: &receivedNTX}, []ids.DepositID{received}},
		{"both, matching", deposits.Filter{
			Status: ptr(deposits.StatusReceived), NetworkTransactionID: &receivedNTX,
		}, []ids.DepositID{received}},
		{"both, not matching", deposits.Filter{
			Status: ptr(deposits.StatusCreated), NetworkTransactionID: &receivedNTX,
		}, nil},
	}
	for _, tt := range tests {
		page, found, err := deposits.ListPage(ctx, pool, account.ID, 10, nil, tt.filter)
		if err != nil || !found {
			t.Fatalf("%s: found %v, err %v", tt.name, found, err)
		}
		if got := idsOf(page); !slices.Equal(got, tt.want) || page.NextBefore != nil {
			t.Errorf("%s: %v, next %v; want %v, nil", tt.name, got, page.NextBefore, tt.want)
		}
	}

	// Another account's deposit isn't found by its network_transaction_id.
	other := testdb.NewAccount(t, pool)
	page, _, err := deposits.ListPage(ctx, pool, other.ID, 10, nil, deposits.Filter{NetworkTransactionID: &receivedNTX})
	if err != nil || len(page.Deposits) != 0 {
		t.Errorf("another account: %v, %v; want none", idsOf(page), err)
	}
}

// limit and before apply within the filtered list, and NextBefore is the last deposit of the
// filtered page: following it skips the deposits the filter leaves out.
func TestListPagePagesThroughAFilteredList(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	var received []ids.DepositID // newest first
	for range 5 {
		deposit := testdb.NewDeposit(t, pool, account, 1250)
		moveTo(t, pool, deposit, deposits.Book)
		received = slices.Insert(received, 0, deposit)
		testdb.NewDeposit(t, pool, account, 1250) // left created, between them
	}

	filter := deposits.Filter{Status: ptr(deposits.StatusReceived)}
	var seen []ids.DepositID
	var cursors []*ids.DepositID
	var before *ids.DepositID
	for {
		page, _, err := deposits.ListPage(ctx, pool, account.ID, 2, before, filter)
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
	if !slices.Equal(seen, received) {
		t.Errorf("seen %v\nwant %v", seen, received)
	}
	if len(cursors) != 3 || cursors[0] == nil || *cursors[0] != received[1] ||
		cursors[1] == nil || *cursors[1] != received[3] || cursors[2] != nil {
		t.Errorf("cursors %v, want the 2nd and 4th received deposits, then nil", cursors)
	}
}

// deposits.GetMany: the account's deposits among the IDs, in one statement; another account's and
// an unknown ID simply absent.
func TestGetManyReadsTheAccountsDepositsByID(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	other := testdb.NewAccount(t, pool)
	first := testdb.NewDeposit(t, pool, account, 1250)
	second := testdb.NewDeposit(t, pool, account, 1250)
	notThis := testdb.NewDeposit(t, pool, other, 1250)

	got, err := deposits.GetMany(ctx, pool, account.ID,
		[]ids.DepositID{second, notThis, ids.New[ids.Deposit](), first})
	if err != nil {
		t.Fatal(err)
	}
	var found []ids.DepositID
	for _, d := range got {
		found = append(found, d.ID)
	}
	slices.SortFunc(found, func(a, b ids.DepositID) int { return cmp.Compare(a.TypeID(), b.TypeID()) })
	if !slices.Equal(found, []ids.DepositID{first, second}) {
		t.Errorf("GetMany = %v, want [first second] (another account's and an unknown ID absent)", found)
	}
	// The rows as the other reads give them.
	one, _, err := deposits.Get(ctx, pool, account.ID, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got {
		if d.ID == first && !reflect.DeepEqual(d, one) {
			t.Errorf("GetMany's row %+v\nGet's        %+v", d, one)
		}
	}

	if none, err := deposits.GetMany(ctx, pool, account.ID, nil); err != nil || len(none) != 0 {
		t.Errorf("GetMany(none) = %v, %v; want nothing and no query's error", none, err)
	}
}
