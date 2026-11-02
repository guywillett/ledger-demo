package withdrawals_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// The entity's own rules. Its lifecycle, with real withdrawals and their funds, is tested in package
// ledger (withdrawals_test.go), which creates them: this package can't (decision 57).

func TestParseStatus(t *testing.T) {
	for _, s := range []string{"held", "submitted", "executed", "failed"} {
		if got, err := withdrawals.ParseStatus(s); err != nil || string(got) != s {
			t.Errorf("ParseStatus(%q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "Held", "settled"} {
		if _, err := withdrawals.ParseStatus(s); err == nil {
			t.Errorf("ParseStatus(%q) succeeded, want an error", s)
		}
	}
}

// The rules Node's types state and Go checks at run time: a submission needs a transfer ID, a
// release a reason, and the zero Transition is none of the three. Checked before any SQL runs, so
// any withdrawal ID will do.
func TestRejectsTransitionsMissingTheirValues(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	tests := []struct {
		name string
		t    withdrawals.Transition
		want string
	}{
		{"a submission without a transfer ID", withdrawals.Submit(""), "transfer ID"},
		{"a release without a reason", withdrawals.Release("", "tr_1"), "reason"},
		{"the zero Transition", withdrawals.Transition{}, "invalid withdrawal transition"},
	}
	for _, tt := range tests {
		err := db.InTx(ctx, pool, func(tx db.Tx) error {
			_, _, err := withdrawals.TransitionStatus(ctx, tx, ids.New[ids.Withdrawal](), tt.t)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestTransitionStatusSkipsAnUnknownWithdrawal(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		_, applied, err := withdrawals.TransitionStatus(ctx, tx, ids.New[ids.Withdrawal](), withdrawals.Execute("tr_1"))
		if applied {
			t.Error("applied, want skipped")
		}
		return err
	})
	if err != nil {
		t.Errorf("error = %v, want an ordinary skip", err)
	}
}

func TestGetMissing(t *testing.T) {
	pool := testdb.Pool(t)
	if _, ok, err := withdrawals.Get(context.Background(), pool, ids.New[ids.Withdrawal]()); ok || err != nil {
		t.Errorf("Get = %v, %v; want not found", ok, err)
	}
}

func TestAsAccountNotFound(t *testing.T) {
	account := ids.New[ids.Account]()
	fk := &pgconn.PgError{Code: "23503", ConstraintName: "withdrawals_account_id_fkey"}
	notFound, ok := errors.AsType[*withdrawals.AccountNotFoundError](withdrawals.AsAccountNotFound(fk, account))
	if !ok || notFound.Account != account || !errors.Is(notFound, fk) {
		t.Errorf("the account's foreign key: got %v, want an AccountNotFoundError wrapping it", notFound)
	}
	for _, other := range []error{
		&pgconn.PgError{Code: "23503", ConstraintName: "some_other_fkey"},
		&pgconn.PgError{Code: "23514", ConstraintName: "withdrawals_account_id_fkey"},
		errors.New("connection refused"),
	} {
		got := withdrawals.AsAccountNotFound(other, account)
		if _, converted := errors.AsType[*withdrawals.AccountNotFoundError](got); converted || !errors.Is(got, other) {
			t.Errorf("AsAccountNotFound(%v) = %v, want it unchanged", other, got)
		}
	}
}
