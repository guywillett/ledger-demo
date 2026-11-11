package db_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// Tx.AfterCommit against the test database: what runs only once a transaction has committed
// (slice 5: counting an applied transition). Node: the afterCommit tests in test/shared/db.test.ts.

func TestAfterCommitHooksRunAfterTheCommitInTheOrderQueued(t *testing.T) {
	pool := testdb.Pool(t)
	var ran []string
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		tx.AfterCommit(func() { ran = append(ran, "first") })
		tx.AfterCommit(func() { ran = append(ran, "second") })
		if _, err := tx.Exec(context.Background(), "SELECT 1"); err != nil {
			return err
		}
		if len(ran) != 0 {
			t.Errorf("ran %v before the commit", ran)
		}
		return nil
	})
	if err != nil || !slices.Equal(ran, []string{"first", "second"}) {
		t.Errorf("err %v, ran %v; want nil, [first second]", err, ran)
	}
}

func TestAfterCommitHooksNeverRunAfterARollback(t *testing.T) {
	pool := testdb.Pool(t)
	ran := false
	rolledBack := errors.New("rolled back")
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		tx.AfterCommit(func() { ran = true })
		return rolledBack
	})
	if !errors.Is(err, rolledBack) || ran {
		t.Errorf("err %v, ran %v; want the rollback's error and no hook", err, ran)
	}
}

func TestAfterCommitHooksNeverRunWhenTheCommitFails(t *testing.T) {
	pool := testdb.Pool(t)
	ran := false
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		tx.AfterCommit(func() { ran = true })
		// A deferred constraint violation surfaces at COMMIT, failing it.
		ctx := context.Background()
		if _, err := tx.Exec(ctx,
			`CREATE TEMP TABLE t (id int PRIMARY KEY DEFERRABLE INITIALLY DEFERRED) ON COMMIT DROP`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO t VALUES (1), (1)`)
		return err
	})
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pgErr.Code != "23505" || ran {
		t.Errorf("err %v, ran %v; want the commit's unique violation and no hook", err, ran)
	}
}

func TestAPanickingHookDoesntFailInTxAndTheLaterHooksStillRun(t *testing.T) {
	pool := testdb.Pool(t)
	var ran []string
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		tx.AfterCommit(func() { panic("a broken hook") })
		tx.AfterCommit(func() { ran = append(ran, "after") })
		return nil
	})
	if err != nil || !slices.Equal(ran, []string{"after"}) {
		t.Errorf("err %v, ran %v; want nil, [after]", err, ran)
	}
}

func TestInTxValueRunsTheHooksToo(t *testing.T) {
	pool := testdb.Pool(t)
	ran := false
	value, err := db.InTxValue(context.Background(), pool, func(tx db.Tx) (string, error) {
		tx.AfterCommit(func() { ran = true })
		return "committed", nil
	})
	if err != nil || value != "committed" || !ran {
		t.Errorf("value %q, err %v, ran %v", value, err, ran)
	}
}

// queue queues a hook on its own copy of tx, as every function taking a db.Tx (by value) does.
func queue(tx db.Tx, hook func()) { tx.AfterCommit(hook) }

func TestAHookQueuedOnACopyOfTheTxStillRuns(t *testing.T) {
	pool := testdb.Pool(t)
	ran := false
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		queue(tx, func() { ran = true })
		return nil
	})
	if err != nil || !ran {
		t.Errorf("err %v, ran %v; want the copy's hook run", err, ran)
	}
}

// A Tx that InTx didn't make (its zero value, the one another package can write) has no queue:
// a programmer error, as Node's afterCommit throws outside withTransaction.
func TestAfterCommitPanicsOnATxThatInTxDidntMake(t *testing.T) {
	defer func() {
		if r := recover(); r != "db: AfterCommit outside InTx" {
			t.Errorf("recovered %v, want the panic", r)
		}
	}()
	db.Tx{}.AfterCommit(func() {})
}
