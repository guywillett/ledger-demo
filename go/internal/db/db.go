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

// Connect returns a connection pool for databaseURL. Like Node's pg.Pool it connects lazily: the
// first query opens the first connection, so a database that is down shows up there.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		// pgx's error doesn't include the URL (and so not its password).
		return nil, fmt.Errorf("database pool: %w", err)
	}
	return pool, nil
}

// Tx is a connection with a transaction open on it. Its field is unexported, so only InTx (in
// this package) can make one: a function that takes a Tx can rely on running inside a
// transaction, as Node's branded Tx guarantees. A plain pgx.Tx could come from anywhere.
type Tx struct {
	tx pgx.Tx
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

	if err := fn(Tx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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
