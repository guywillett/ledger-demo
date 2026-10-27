package db_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// The Go tests' own database, migrated by the compose service migrate-go-test, so the Node and Go
// suites never share rows (slice 3 decision 52).
//
// `go test ./...` runs the packages' tests in parallel, as separate processes, all against this
// one database. Nothing conflicts yet: each test creates its own rows with fresh IDs. The outbox
// tests (step 5) will: claiming takes the oldest unpublished events in the whole table, as in
// Node, where those tests run last and one file at a time.
const defaultTestDatabaseURL = "postgres://ledger:ledger@localhost:5432/ledger_go_test?sslmode=disable"

// testPool connects to TEST_DATABASE_URL (default: the compose ledger_go_test), refusing any
// database whose name doesn't end in _test, as Node's vitest.config.ts does. The pool is closed
// when the test ends (t.Cleanup, the counterpart of Vitest's afterAll).
//
// Only this package needs a database so far; when others do (step 3), this moves to a small
// package of its own.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = defaultTestDatabaseURL
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL is not a URL")
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to run tests against database %q: its name must end in _test", name)
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("test database unreachable (is `docker compose up -d` running?): %v", err)
	}
	return pool
}

// insertAccount inserts an account with a fresh ID and unique name and IBAN.
func insertAccount(ctx context.Context, q db.Querier) (ids.AccountID, error) {
	id := ids.New[ids.Account]()
	// The ID's 26 characters make the name and IBAN unique; the IBAN only has to pass the table's
	// format CHECK, not a checksum.
	suffix := strings.ToUpper(strings.TrimPrefix(id.TypeID(), "acct_"))
	_, err := q.Exec(ctx, `INSERT INTO accounts (id, name, iban) VALUES ($1, $2, $3)`,
		id, "go-test-"+suffix, "XX00"+suffix)
	return id, err
}

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
	pool := testPool(t)
	ctx := context.Background()
	var id ids.AccountID
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		var err error
		id, err = insertAccount(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("InTx error: %v", err)
	}
	if !accountExists(t, pool, id) {
		t.Error("the account isn't there after a commit")
	}
}

var errBoom = errors.New("boom")

func TestRollsBackOnError(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var id ids.AccountID
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		var err error
		if id, err = insertAccount(ctx, tx); err != nil {
			return err
		}
		return fmt.Errorf("after inserting: %w", errBoom)
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("InTx error = %v, want errBoom (wrapped)", err)
	}
	if accountExists(t, pool, id) {
		t.Error("the account is there after a rollback")
	}
}

func TestRollsBackOnPanicAndRePanics(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var id ids.AccountID
	// recover() inside a deferred function stops a panic and returns its value: the counterpart
	// of catch, used here only to observe that the panic got through InTx.
	recovered := func() (r any) {
		defer func() { r = recover() }()
		_ = db.InTx(ctx, pool, func(tx db.Tx) error {
			var err error
			if id, err = insertAccount(ctx, tx); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
		return nil
	}()
	if recovered != "boom" {
		t.Fatalf("recovered %v, want the panic to reach the caller", recovered)
	}
	if accountExists(t, pool, id) {
		t.Error("the account is there after a panic")
	}
}

// An ID is stored as its UUID and read back as the same ID (ids' Value and Scan, through pgx).
func TestIDsAreStoredAsUUIDs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	id, err := insertAccount(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	var back ids.AccountID
	err = pool.QueryRow(ctx, `SELECT id::text, id FROM accounts WHERE id = $1`, id).Scan(&stored, &back)
	if err != nil {
		t.Fatal(err)
	}
	if stored != id.UUID().String() || back != id {
		t.Errorf("stored %s, read back %v; want %s and %v", stored, back, id.UUID(), id)
	}
}
