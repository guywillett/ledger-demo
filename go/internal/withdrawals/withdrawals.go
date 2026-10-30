// Package withdrawals is the withdrawal entity: its statuses, its legal transitions and their SQL,
// and how it is read. Node: node/src/withdrawals/status.ts and withdrawals.ts.
//
// The entity layer (decision 57): it imports no ledger. Creating a withdrawal is the ledger's job
// (ledger.CreateWithdrawal inserts it and holds its funds in one go), so this package has no
// insert: a withdrawal can't be stored without its hold.
//
// The lifecycle:
//
//	held -> submitted -> executed
//	  |         \-----> failed
//	  |---------------> executed   (the provider's update arrived before our submit response)
//	  \---------------> failed     (refused at submission, or the provider's update came first)
//
// executed and failed are final. Status names are part of the event contract (withdrawal.<status>).
package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
)

// Status is a withdrawal's status.
type Status string

const (
	StatusHeld      Status = "held"
	StatusSubmitted Status = "submitted"
	StatusExecuted  Status = "executed"
	StatusFailed    Status = "failed"
)

// ParseStatus checks a status read from the database.
func ParseStatus(s string) (Status, error) {
	switch status := Status(s); status {
	case StatusHeld, StatusSubmitted, StatusExecuted, StatusFailed:
		return status, nil
	}
	return "", fmt.Errorf("unknown withdrawal status in database: %q", s)
}

// TransferID is the provider's ID for our transfer, "" for none given. Its own type so it can't be
// swapped with another string argument, such as Release's reason: two adjacent string parameters
// would compile either way round. Untyped string constants ("tr_1") convert to it implicitly, so
// literals aren't protected; values are, and a transfer ID arrives as a value (from the provider's
// answer or its update).
type TransferID string

// Withdrawal is a stored withdrawal. The nullable columns are pointers (nil for NULL): pgx scans
// NULL into a nil pointer, and a pointer keeps "no value" apart from any string.
type Withdrawal struct {
	ID                 ids.WithdrawalID
	Account            ids.AccountID
	IdempotencyKey     string
	Amount             money.Amount
	Currency           string
	CreditorIBAN       sepa.IBAN
	CreditorName       string
	RemittanceInfo     *string
	Status             Status
	FailureReason      *string
	ProviderTransferID *TransferID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Columns lists a withdrawal's columns in the order Scan reads them, for every statement that
// returns a withdrawal (here, and the ledger's insert). Explicit rather than Node's SELECT *: Go
// scans by position, and * would follow the table's column order.
const Columns = `id, account_id, idempotency_key, amount_minor, currency, creditor_iban,
creditor_name, remittance_info, status, failure_reason, provider_transfer_id, created_at,
updated_at`

// Scan reads one row of Columns. pgx.Row is the interface QueryRow returns (one method, Scan), so
// Scan takes the result of any QueryRow, on the pool or a transaction.
func Scan(row pgx.Row) (Withdrawal, error) {
	var w Withdrawal
	var iban, status string
	err := row.Scan(&w.ID, &w.Account, &w.IdempotencyKey, &w.Amount, &w.Currency, &iban,
		&w.CreditorName, &w.RemittanceInfo, &status, &w.FailureReason, &w.ProviderTransferID,
		&w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return Withdrawal{}, err // pgx.ErrNoRows included: callers check for it
	}
	if w.Currency != "EUR" {
		return Withdrawal{}, fmt.Errorf("withdrawal %v: unexpected currency in database: %q",
			w.ID, w.Currency)
	}
	// Stored only after sepa.ParseIBAN, and the column's CHECK guards the format.
	w.CreditorIBAN = sepa.IBAN(iban)
	if w.Status, err = ParseStatus(status); err != nil {
		return Withdrawal{}, fmt.Errorf("withdrawal %v: %w", w.ID, err)
	}
	return w, nil
}

// NewWithdrawal is what creating a withdrawal needs (ledger.CreateWithdrawal).
type NewWithdrawal struct {
	Account        ids.AccountID
	IdempotencyKey string
	Amount         money.Amount
	CreditorIBAN   sepa.IBAN
	CreditorName   string
	RemittanceInfo *string // nil for none
}

const getSQL = `SELECT ` + Columns + ` FROM withdrawals WHERE id = $1`

const byKeySQL = `SELECT ` + Columns + `
FROM withdrawals WHERE account_id = $1 AND idempotency_key = $2`

// Get reads a withdrawal; false if there is none.
func Get(ctx context.Context, q db.Querier, id ids.WithdrawalID) (Withdrawal, bool, error) {
	return one(Scan(q.QueryRow(ctx, getSQL, id)))
}

// GetByKey reads the withdrawal holding an account's idempotency key. For classifying a request
// whose insert found the key taken; never used to decide whether to insert.
func GetByKey(
	ctx context.Context,
	q db.Querier,
	account ids.AccountID,
	key string,
) (Withdrawal, bool, error) {
	return one(Scan(q.QueryRow(ctx, byKeySQL, account, key)))
}

// one turns pgx.ErrNoRows into Go's comma-ok form.
func one(w Withdrawal, err error) (Withdrawal, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, nil
	}
	if err != nil {
		return Withdrawal{}, false, err
	}
	return w, true, nil
}

// AccountNotFoundError: the withdrawal's account doesn't exist, and the insert's foreign key
// refused it (no existence read before the insert). Returned, not handled, because the failed
// statement has aborted the transaction: the caller handles it after the rollback (the API answers
// 404).
type AccountNotFoundError struct {
	Account ids.AccountID
	err     error // the database's foreign_key_violation
}

func (e *AccountNotFoundError) Error() string {
	return fmt.Sprintf("account %v does not exist", e.Account)
}

// Unwrap returns the database's error.
func (e *AccountNotFoundError) Unwrap() error { return e.err }

// foreignKeyViolation is Postgres's SQLSTATE for a foreign key failing.
const foreignKeyViolation = "23503"

// AsAccountNotFound turns a foreign_key_violation on the withdrawal's account into an
// *AccountNotFoundError, and returns any other error unchanged. For the ledger's insert, which
// owns the statement; the constraint's name is this table's business, so the mapping lives here.
func AsAccountNotFound(err error, account ids.AccountID) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pgErr.Code != foreignKeyViolation ||
		pgErr.ConstraintName != "withdrawals_account_id_fkey" {
		return err
	}
	return &AccountNotFoundError{Account: account, err: err}
}
