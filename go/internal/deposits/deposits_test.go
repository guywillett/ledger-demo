package deposits_test

import (
	"context"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

func TestParseStatus(t *testing.T) {
	for _, s := range []string{"created", "received", "under_review", "approved", "rejected", "credited", "returned", "unmatched"} {
		if got, err := deposits.ParseStatus(s); err != nil || string(got) != s {
			t.Errorf("ParseStatus(%q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "Created", "booked"} {
		if _, err := deposits.ParseStatus(s); err == nil {
			t.Errorf("ParseStatus(%q) succeeded, want an error", s)
		}
	}
}

// The transitions are the lifecycle, and nothing else.
func TestTransitions(t *testing.T) {
	tests := []struct {
		t        deposits.Transition
		from, to deposits.Status
	}{
		{deposits.Book, deposits.StatusCreated, deposits.StatusReceived},
		{deposits.Credit, deposits.StatusApproved, deposits.StatusCredited},
		{deposits.Return, deposits.StatusRejected, deposits.StatusReturned},
		{deposits.StartReview, deposits.StatusReceived, deposits.StatusUnderReview},
		{deposits.Approve, deposits.StatusUnderReview, deposits.StatusApproved},
		{deposits.Reject, deposits.StatusUnderReview, deposits.StatusRejected},
	}
	for _, tt := range tests {
		if tt.t.From() != tt.from || tt.t.To() != tt.to {
			t.Errorf("%v = %s -> %s, want %s -> %s", tt.t, tt.t.From(), tt.t.To(), tt.from, tt.to)
		}
	}
}

// A Transition that isn't one of the constants (only possible by conversion) changes nothing.
func TestRejectsInvalidTransitions(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	deposit := testdb.NewDeposit(t, pool, account, 1250)
	for _, invalid := range []deposits.Transition{0, 42} {
		err := db.InTx(ctx, pool, func(tx db.Tx) error {
			_, _, err := deposits.TransitionStatus(ctx, tx, deposit, invalid, "")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "invalid deposit transition") {
			t.Errorf("TransitionStatus(%v) error = %v, want invalid transition", invalid, err)
		}
	}
}

func TestTransitionStatus(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	account := testdb.NewAccount(t, pool)
	deposit := testdb.NewDeposit(t, pool, account, 1250)
	run := func() (deposits.Transitioned, bool) {
		var moved deposits.Transitioned
		var applied bool
		err := db.InTx(ctx, pool, func(tx db.Tx) error {
			var err error
			moved, applied, err = deposits.TransitionStatus(ctx, tx, deposit, deposits.Book, "")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return moved, applied
	}
	moved, applied := run()
	if !applied || moved.Account != account.ID || moved.Amount != 1250 {
		t.Errorf("first = %+v, %v; want the account and 1250, applied", moved, applied)
	}
	if _, applied := run(); applied {
		t.Error("second applied, want skipped")
	}
}

func TestKeepsHistoryAppendOnly(t *testing.T) {
	pool := testdb.Pool(t)
	deposit := testdb.NewDeposit(t, pool, testdb.NewAccount(t, pool), 1250)
	ctx := context.Background()
	for statement, want := range map[string]string{
		`UPDATE deposit_status_history SET reason = 'edited' WHERE deposit_id = $1`: "append-only: UPDATE",
		`DELETE FROM deposit_status_history WHERE deposit_id = $1`:                  "append-only: DELETE",
	} {
		if _, err := pool.Exec(ctx, statement, deposit); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", statement, err, want)
		}
	}
}
