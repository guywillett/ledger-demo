package deposits_test

import (
	"context"
	"reflect"
	"testing"

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
	page, _, err := deposits.ListPage(context.Background(), pool, account.ID, 1, nil)
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
