// Package testdb connects tests to the Go test database and creates the rows they need. Imported
// only by tests, like vectortest and the standard library's httptest.
//
// Two databases, so the Node and Go suites never share rows (slice 3 decision 52):
//
//   - Main, ledger_go_test (compose service migrate-go-test), for every package's tests but two.
//     `go test ./...` runs packages in parallel, as separate processes, all against it; they don't
//     conflict, since each test creates its own rows with fresh IDs.
//   - Relay, ledger_go_relay_test (migrate-go-relay-test), for the outbox and relay tests only.
//     Claiming takes the oldest unpublished events in the whole table, so those tests must see no
//     other package's events (the webhook tests leave deposit.created rows behind in Main on every
//     run) and must not run while each other's are running: each of the two packages holds the
//     database's lock for its whole run (Exclusive). Node runs the same tests one file at a time,
//     in its serial "relay" project.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
)

// Database is a test database: the environment variable that can point elsewhere, and its default.
type Database struct {
	envVar     string
	defaultURL string
}

var (
	// Main is ledger_go_test (TEST_DATABASE_URL).
	Main = Database{
		envVar:     "TEST_DATABASE_URL",
		defaultURL: "postgres://ledger:ledger@localhost:5432/ledger_go_test?sslmode=disable",
	}
	// Relay is ledger_go_relay_test (TEST_RELAY_DATABASE_URL), for the outbox and relay tests.
	Relay = Database{
		envVar:     "TEST_RELAY_DATABASE_URL",
		defaultURL: "postgres://ledger:ledger@localhost:5432/ledger_go_relay_test?sslmode=disable",
	}
)

// url is the database's URL, checked: its name must end in _test, as Node's vitest.config.ts
// requires.
func (d Database) url() (*url.URL, error) {
	databaseURL := os.Getenv(d.envVar)
	if databaseURL == "" {
		databaseURL = d.defaultURL
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("%s is not a URL", d.envVar)
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		return nil, fmt.Errorf("refusing to run tests against database %q: its name must end in _test", name)
	}
	return u, nil
}

// Pool connects to Main; see PoolFor.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return PoolFor(t, Main)
}

// PoolFor connects to a test database (refusing any whose name doesn't end in _test). The pool is
// closed when the test ends (t.Cleanup, the counterpart of Vitest's afterAll).
func PoolFor(t testing.TB, d Database) *pgxpool.Pool {
	t.Helper()
	u, err := d.url()
	if err != nil {
		t.Fatal(err)
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

// exclusiveLock is the advisory lock Exclusive holds: any number, the same in every package.
const exclusiveLock = 0x6c656467 // "ledg"

// Exclusive runs a package's tests (m.Run, from its TestMain) holding a lock on the database, so
// no other package holding it runs at the same time, and returns their exit code.
//
// TestMain, if a test package defines one, runs instead of the tests and calls m.Run itself: the
// place for set-up around a whole package (Vitest: globalSetup, or a project's settings). The lock
// is a Postgres advisory lock, held by this connection's session: pg_advisory_lock waits until
// it is free, and Postgres releases it when the connection closes, so a crashed test process can't
// leave it held. If it is taken, a line says so before waiting, so a package stuck behind another
// that hangs says why.
func Exclusive(m *testing.M, d Database) int {
	u, err := d.url()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "test database unreachable (is `docker compose up -d` running?): %v\n", err)
		return 1
	}
	defer func() { _ = conn.Close(ctx) }()
	var locked bool
	err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", exclusiveLock).Scan(&locked)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !locked {
		fmt.Fprintf(os.Stderr, "waiting for the %s lock (another package's tests hold it)\n",
			strings.TrimPrefix(u.Path, "/"))
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", exclusiveLock); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}
