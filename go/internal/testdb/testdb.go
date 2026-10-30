// Package testdb connects tests to the Go test database and creates the rows they need. Imported
// only by tests, like vectortest and the standard library's httptest.
//
// The database is ledger_go_test, migrated by the compose service migrate-go-test, so the Node
// and Go suites never share rows (slice 3 decision 52).
//
// `go test ./...` runs the packages' tests in parallel, as separate processes, all against this
// one database. Nothing conflicts yet: each test creates its own rows with fresh IDs. The outbox
// tests (step 5) will: claiming takes the oldest unpublished events in the whole table, as in
// Node, where those tests run last and one file at a time.
package testdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
)

const defaultURL = "postgres://ledger:ledger@localhost:5432/ledger_go_test?sslmode=disable"

// Pool connects to TEST_DATABASE_URL (default: the compose ledger_go_test), refusing any database
// whose name doesn't end in _test, as Node's vitest.config.ts does. The pool is closed when the
// test ends (t.Cleanup, the counterpart of Vitest's afterAll).
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = defaultURL
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL is not a URL")
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to run tests against database %q: its name must end in _test", name)
	}
	// Up to 10 connections, as Node's pg pool, unless the URL sets its own: pgxpool's default is
	// the number of CPUs (at least 4), and the concurrency tests run 10 transactions at once.
	query := u.Query()
	if !query.Has("pool_max_conns") {
		query.Set("pool_max_conns", "10")
		u.RawQuery = query.Encode()
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("test database unreachable (is `docker compose up -d` running?): %v", err)
	}
	return pool
}

// Account is a test account: its ID and IBAN.
type Account struct {
	ID   ids.AccountID
	IBAN string
}

// InsertAccount inserts an account with a fresh ID and a unique name and IBAN, on q (the pool or
// a transaction). Its balance row (all zero) is created by a trigger, as for every account.
func InsertAccount(ctx context.Context, q db.Querier) (Account, error) {
	id := ids.New[ids.Account]()
	// The ID's 26 characters make the name and IBAN unique; the IBAN only has to pass the table's
	// format CHECK, not a checksum.
	suffix := strings.ToUpper(strings.TrimPrefix(id.TypeID(), "acct_"))
	account := Account{ID: id, IBAN: "XX00" + suffix}
	_, err := q.Exec(ctx, `INSERT INTO accounts (id, name, iban) VALUES ($1, $2, $3)`,
		id, "go-test-"+suffix, account.IBAN)
	return account, err
}

// NewAccount is InsertAccount on the pool, failing the test on an error.
func NewAccount(t testing.TB, pool *pgxpool.Pool) Account {
	t.Helper()
	account, err := InsertAccount(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

// NewDeposit inserts a deposit in status created for the account, with its first history row
// (null -> created), as ingestion records one. Inserted directly with SQL: Go has no ingestion
// yet (step 4). No outbox row: nothing in these tests publishes.
func NewDeposit(
	t testing.TB,
	pool *pgxpool.Pool,
	account Account,
	amount money.Amount,
) ids.DepositID {
	t.Helper()
	ctx := context.Background()
	id := ids.New[ids.Deposit]()
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO deposits (id, creditor_iban, account_id, amount_minor, currency, network,
                      network_transaction_id, status)
VALUES ($1, $2, $3, $4, 'EUR', 'SEPA', $5, 'created')`,
			id, account.IBAN, account.ID, amount, "go-test-"+id.TypeID())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
INSERT INTO deposit_status_history (deposit_id, from_status, to_status)
VALUES ($1, NULL, 'created')`, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
