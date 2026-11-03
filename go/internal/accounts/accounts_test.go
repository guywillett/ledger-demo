package accounts_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/accounts"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// accounts.Get, against the test database: an account found by its ID, and none for an unknown ID.

func TestGetReadsAnAccount(t *testing.T) {
	pool := testdb.Pool(t)
	account := testdb.NewAccount(t, pool)
	var createdAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT created_at FROM accounts WHERE id = $1`, account.ID).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}

	got, found, err := accounts.Get(context.Background(), pool, account.ID)
	if err != nil || !found {
		t.Fatalf("Get = found %v, err %v; want the account", found, err)
	}
	// testdb.InsertAccount names it after its ID: "go-test-" and the ID's suffix, upper case.
	name := "go-test-" + strings.ToUpper(strings.TrimPrefix(account.ID.TypeID(), "acct_"))
	want := accounts.Account{
		ID: account.ID, Name: name, IBAN: sepa.IBAN(account.IBAN), CreatedAt: createdAt,
	}
	if got.ID != want.ID || got.Name != want.Name || got.IBAN != want.IBAN ||
		!got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("Get = %+v\nwant  %+v", got, want)
	}
}

func TestGetReadsASeededAccount(t *testing.T) {
	alice, err := ids.FromTypeID[ids.Account]("acct_01m564xr00fx7rjvqryn5dzwxx")
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := accounts.Get(context.Background(), testdb.Pool(t), alice)
	if err != nil || !found || got.Name != "alice" || got.IBAN != "FR7612345000010000000000193" {
		t.Errorf("Get(alice) = %+v, found %v, err %v", got, found, err)
	}
}

func TestGetReportsAMissingAccountAsNotFound(t *testing.T) {
	_, found, err := accounts.Get(context.Background(), testdb.Pool(t), ids.New[ids.Account]())
	if err != nil || found {
		t.Errorf("Get(unknown) = found %v, err %v; want not found, no error", found, err)
	}
}
