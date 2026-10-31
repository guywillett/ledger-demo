package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

func accountExists(t *testing.T, pool *pgxpool.Pool, id ids.AccountID) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, id).
		Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestCommits(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	var account testdb.Account
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		var err error
		account, err = testdb.InsertAccount(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("InTx error: %v", err)
	}
	if !accountExists(t, pool, account.ID) {
		t.Error("the account isn't there after a commit")
	}
}

var errBoom = errors.New("boom")

func TestRollsBackOnError(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	var account testdb.Account
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		var err error
		if account, err = testdb.InsertAccount(ctx, tx); err != nil {
			return err
		}
		return fmt.Errorf("after inserting: %w", errBoom)
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("InTx error = %v, want errBoom (wrapped)", err)
	}
	if accountExists(t, pool, account.ID) {
		t.Error("the account is there after a rollback")
	}
}

func TestRollsBackOnPanicAndRePanics(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	var account testdb.Account
	// recover() inside a deferred function stops a panic and returns its value: the counterpart
	// of catch, used here only to observe that the panic got through InTx.
	recovered := func() (r any) {
		defer func() { r = recover() }()
		_ = db.InTx(ctx, pool, func(tx db.Tx) error {
			var err error
			if account, err = testdb.InsertAccount(ctx, tx); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
		return nil
	}()
	if recovered != "boom" {
		t.Fatalf("recovered %v, want the panic to reach the caller", recovered)
	}
	if accountExists(t, pool, account.ID) {
		t.Error("the account is there after a panic")
	}
}

// An ID is stored as its UUID and read back as the same ID (ids' Value and Scan, through pgx).
func TestIDsAreStoredAsUUIDs(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	id := testdb.NewAccount(t, pool).ID
	var stored string
	var back ids.AccountID
	err := pool.QueryRow(ctx, `SELECT id::text, id FROM accounts WHERE id = $1`, id).Scan(&stored, &back)
	if err != nil {
		t.Fatal(err)
	}
	if stored != id.UUID().String() || back != id {
		t.Errorf("stored %s, read back %v; want %s and %v", stored, back, id.UUID(), id)
	}
}

// InTxValue returns fn's value once the transaction commits, and the zero value with the error when
// it rolls back.
func TestInTxValueReturnsTheValueOnCommitAndTheZeroValueOnRollback(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	got, err := db.InTxValue(ctx, pool, func(tx db.Tx) (int, error) {
		var n int
		err := tx.QueryRow(ctx, `SELECT 41 + 1`).Scan(&n)
		return n, err
	})
	if err != nil || got != 42 {
		t.Errorf("commit: %d, %v", got, err)
	}
	failed := errors.New("the step failed")
	got, err = db.InTxValue(ctx, pool, func(db.Tx) (int, error) { return 7, failed })
	if !errors.Is(err, failed) || got != 0 {
		t.Errorf("rollback: %d, %v (want 0 and the error)", got, err)
	}
}
