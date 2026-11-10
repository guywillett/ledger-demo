// Package db connects to Postgres with pgx and runs transactions. The Node counterparts
// (node/src/shared/db.ts): Querier is Queryable, Tx is the branded Tx, InTx is withTransaction.
//
// Callers own transaction boundaries (CLAUDE.md): data-access functions never begin or commit.
// A function whose statements must be atomic together takes a Tx; a single-statement function
// takes a Querier, so it runs on the pool or inside a caller's transaction.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier runs statements: the pool (each on whichever connection is free) or a Tx.
//
// An interface in Go is satisfied implicitly: any type with these three methods is a Querier,
// without declaring it, much as a TypeScript type is satisfied by any object of the right shape.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Compile-time checks that the pool and Tx are Queriers: assigning to the blank identifier _
// costs nothing at run time, but fails the build if a method goes missing.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = Tx{}
)

// Ping reports whether the database answers SELECT 1 on q. On the pool, acquiring a connection is
// part of it, and ctx's deadline bounds both the wait for a connection and the query: a pool too
// busy to lend one in time fails too, which is what GET /health relies on (slice 4 decision 72:
// readiness through the main pool). Node: ping in node/src/shared/db.ts.
func Ping(ctx context.Context, db Querier) error {
	var one int
	return db.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// Connect returns a connection pool for databaseURL. Like Node's pg.Pool it connects lazily: the
// first query opens the first connection, so a database that is down shows up there. Its statements
// are traced (queryTracer: spans only inside a span, and only with telemetry on).
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx's error doesn't include the URL (and so not its password).
		return nil, fmt.Errorf("database pool: %w", err)
	}
	config.ConnConfig.Tracer = queryTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		// pgx's error doesn't include the URL (and so not its password).
		return nil, fmt.Errorf("database pool: %w", err)
	}
	return pool, nil
}

// afterCommit is a transaction's after-commit queue: the functions Tx.AfterCommit queues and InTx
// runs once it has committed. One per transaction, shared by every copy of its Tx (see Tx.hooks).
type afterCommit struct {
	fns []func()
}

// Tx is a connection with a transaction open on it. Its fields are unexported, so only InTx (in
// this package) can make one: a function that takes a Tx can rely on running inside a
// transaction, as Node's branded Tx guarantees. A plain pgx.Tx could come from anywhere.
type Tx struct {
	tx pgx.Tx
	// The functions queued by AfterCommit, behind a pointer that every copy of the Tx shares. A Tx
	// is passed by value (fn's tx, and every data-access function's), so a plain []func() field
	// would be copied with it: an append on a copy changes only that copy's slice header (its
	// length; even when it writes into the shared array), and InTx, reading its own Tx, would see
	// nothing queued. A pointer receiver on AfterCommit wouldn't help either: it would change the
	// caller's copy, not InTx's. Copying the struct copies the pointer, so every copy appends to the
	// one afterCommit InTx made (Go dereferences the pointer for t.hooks.fns: no * needed).
	hooks *afterCommit
}

// Exec runs a statement that returns no rows.
func (t Tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

// Query runs a statement that returns rows.
func (t Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.tx.Query(ctx, sql, args...)
}

// QueryRow runs a statement that returns at most one row.
func (t Tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

// AfterCommit queues f to run once the transaction has committed: after a successful Commit, in
// the order queued; never after a rollback (fn's error, or its panic) nor a failed Commit. For what
// must follow only a committed change and can't be part of it: counting an applied transition
// (slice 5 decision 88), so a rolled-back transaction counts nothing. f runs after the
// transaction, outside it: no statements in it (the connection is back in the pool). A panicking f
// is recovered and logged, the others still run, and InTx still returns nil: the transaction has
// committed, and nothing can undo that. Node: afterCommit in node/src/shared/db.ts.
//
// At most once: a crash after the commit skips it, so never for anything that must happen;
// that's the outbox's job.
//
// A value receiver, as Tx's other methods: the queue is shared through the pointer (see Tx).
//
// Panics on a Tx that InTx didn't make: another package can't set Tx's fields, but it can write
// its zero value, db.Tx{}, which has no queue. A programmer error, as Node's afterCommit throws
// outside withTransaction.
func (t Tx) AfterCommit(f func()) {
	if t.hooks == nil {
		panic("db: AfterCommit outside InTx")
	}
	t.hooks.fns = append(t.hooks.fns, f)
}

// InTx runs fn in a transaction on one connection: COMMIT if fn returns nil, ROLLBACK if it
// returns an error (returned as is, so errors.Is still matches it) or panics (the panic carries
// on after the rollback). A value fn computes is returned through a variable it closes over:
//
//	var balance money.Amount
//	err := db.InTx(ctx, pool, func(tx db.Tx) error {
//		var err error
//		balance, err = hold(ctx, tx, …)
//		return err
//	})
//
// Never call InTx inside fn: that opens a second, independent transaction on another connection.
//
// After a successful Commit, InTx runs the functions fn queued with tx.AfterCommit (InTxValue, which
// calls it, too).
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// defer runs this when InTx returns, however it returns, including while a panic unwinds the
	// stack (the panic then continues to the caller): the Go counterpart of finally. After a
	// successful Commit, Rollback does nothing (pgx returns ErrTxClosed, ignored), so this one
	// call covers an error from fn, a failed Commit and a panic.
	//
	// WithoutCancel: if ctx has been cancelled (e.g. a shutdown), the rollback must still be sent
	// rather than fail at once; pgx closes the connection if it can't complete.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// The queue starts empty: a nil fns, which append grows and range reads as no hooks.
	t := Tx{tx: tx, hooks: &afterCommit{}}
	if err := fn(t); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	// Committed: the hooks, in order, each on its own (runHook).
	for _, hook := range t.hooks.fns {
		runHook(hook)
	}
	return nil
}

// runHook runs one after-commit hook, recovering its panic: the transaction has committed, so a
// panic escaping InTx would only turn a committed change into a failure for its caller, and skip
// the later hooks. A function of its own because a defer belongs to the function, not to a loop's
// iteration: a recovering defer in InTx's loop would run only when InTx returned, after the
// remaining hooks had been skipped.
//
// Logged through slog.Default(): package db has no logger of its own (neither Connect nor InTx
// takes one). That is the process's JSON logger once the process has set it (cmd/api, cmd/relay
// and consumer.Run do, after making it), without the delivery's or the request's fields; before
// that, the standard library's text on stderr. Enough for something that should never happen,
// with alert: true to be found.
func runHook(hook func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Error("after-commit hook failed; the transaction stands",
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()), "alert", true)
		}
	}()
	hook()
}

// InTxValue is InTx for a transaction that computes a value: fn returns it with its error, and
// InTxValue returns it once the transaction has committed (the zero value if it failed). A generic
// function (T is the value's type), so each caller keeps its own result type: the deposit-worker's
// and the submitter's steps each run in a transaction of their own and return what they did.
func InTxValue[T any](
	ctx context.Context,
	pool *pgxpool.Pool,
	fn func(tx Tx) (T, error),
) (T, error) {
	var value T
	err := InTx(ctx, pool, func(tx Tx) error {
		var err error
		value, err = fn(tx)
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return value, nil
}
