// Package ingestion records provider notifications as deposits, and a new deposit's
// deposit.created event in the outbox. Node: node/src/ingestion/incoming-payments.ts and events.ts.
// This is the edge where provider identifiers (IBANs, provider account IDs) are mapped to our
// account; nothing downstream sees them. It never touches balances.
//
// The orchestration layer (decision 57): it uses the deposits and outbox entities and the events'
// wire format, and the API calls it.
//
// Nothing is refused (the webhook contract, slice 2 decisions 37-38): a notification for an IBAN
// that matches none of our accounts is recorded in suspense (status unmatched), and one that
// conflicts with the stored deposit is recorded as a conflict. Both change no balance.
package ingestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/outbox"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// Notification is a provider's notification of an incoming payment, validated (the API decodes
// it from the webhook body).
type Notification struct {
	CreditorIBAN         sepa.IBAN
	ProviderAccountID    *string // nil when the provider gave none
	Amount               money.Amount
	Currency             string // "EUR"
	Network              string // "SEPA"
	NetworkTransactionID string
}

// ComparedField is a detail that must match for a repeat notification to count as a duplicate.
type ComparedField string

const (
	FieldCreditorIBAN ComparedField = "creditor_iban"
	FieldAmountMinor  ComparedField = "amount_minor"
	FieldCurrency     ComparedField = "currency"
)

// Details are the compared details of a notification or a stored deposit.
type Details struct {
	CreditorIBAN sepa.IBAN
	Amount       money.Amount
	Currency     string
}

// DifferingFields returns the fields in which a repeated notification differs from the stored
// deposit ([] = a duplicate). It must compare exactly the fields the upsert's ON CONFLICT … WHERE
// compares. Exported for the shared vectors (contracts/vectors/deposit-same-details.json), which
// pin the rule for both implementations; Node: differingDepositFields.
func DifferingFields(stored, notification Details) []ComparedField {
	fields := []ComparedField{}
	if stored.CreditorIBAN != notification.CreditorIBAN {
		fields = append(fields, FieldCreditorIBAN)
	}
	if stored.Amount != notification.Amount {
		fields = append(fields, FieldAmountMinor)
	}
	if stored.Currency != notification.Currency {
		fields = append(fields, FieldCurrency)
	}
	return fields
}

func (n Notification) details() Details {
	return Details{CreditorIBAN: n.CreditorIBAN, Amount: n.Amount, Currency: n.Currency}
}

// Kind is what recording a notification did.
type Kind string

const (
	// A new deposit for one of our accounts, with its deposit.created event in the outbox.
	Created Kind = "created"
	// The same notification again, for a matched or an unmatched deposit: counted.
	Duplicate Kind = "duplicate"
	// A new notification for an IBAN that matches none of our accounts: recorded in suspense.
	Unmatched Kind = "unmatched"
	// The network ID is stored with other details: recorded in deposit_notification_conflicts.
	Conflict Kind = "conflict"
	// The statement refused to record the notification, a deposit with this network ID is stored,
	// yet no compared field differs. Not the provider's error: either the SQL rule and
	// DifferingFields have drifted apart, or an account's IBAN changed (which must never happen).
	Inconsistent Kind = "inconsistent"
)

// Result is what RecordNotification did, with what the caller logs. Fields that don't apply to
// the Kind are zero.
type Result struct {
	Kind Kind
	// The deposit recorded or counted (Created, Duplicate, Unmatched) or the stored one it
	// conflicts with (Conflict, Inconsistent).
	Deposit           ids.DepositID
	Status            deposits.Status // Duplicate
	NotificationCount int             // Duplicate
	Event             ids.EventID     // Created: the deposit.created event in the outbox
	DifferingFields   []ComparedField // Conflict
}

// One statement records the notification: match the IBAN to our account (or none) and insert the
// deposit, created or unmatched, with its first history row; or recognise a duplicate (same
// details: count it). It returns the deposit in both cases, and no row when nothing was recorded:
// the network ID is already used with different details (conflict). The account is a LEFT JOIN,
// so an unknown IBAN still inserts, with account_id NULL and status unmatched.
//
// Postgres 18: RETURNING can reference old and new. old is NULL for a row that was just
// inserted, so (old.id IS NULL) means "inserted". Data-modifying CTEs always run to completion,
// so `history` runs although nothing reads it. Node's statement exactly.
const recordNotificationSQL = `
WITH upsert AS (
  INSERT INTO deposits (id, creditor_iban, provider_account_id, account_id,
                                 amount_minor, currency, network, network_transaction_id, status)
  SELECT $1, $2, $3, a.id, $4, $5, $6, $7,
         CASE WHEN a.id IS NULL THEN 'unmatched' ELSE 'created' END
  FROM (SELECT $2::text AS iban) n
  LEFT JOIN accounts a ON a.iban = n.iban
  ON CONFLICT (network, network_transaction_id) DO UPDATE
    SET notification_count = deposits.notification_count + 1,
        updated_at = now()
    WHERE (deposits.creditor_iban, deposits.amount_minor,
           deposits.currency)
        = (EXCLUDED.creditor_iban, EXCLUDED.amount_minor, EXCLUDED.currency)
  RETURNING new.*, (old.id IS NULL) AS inserted
),
history AS (
  INSERT INTO deposit_status_history (deposit_id, from_status, to_status)
  SELECT id, NULL, status FROM upsert WHERE inserted
)
SELECT * FROM upsert
`

// upsertRow is the statement's row. The db tags name its columns: pgx.RowToStructByName fills the
// fields by column name, not position, since new.* follows the table's column order. Every column
// must have a field (pgx refuses a row with a column it can't place).
type upsertRow struct {
	ID                   ids.DepositID  `db:"id"`
	CreditorIBAN         string         `db:"creditor_iban"`
	ProviderAccountID    *string        `db:"provider_account_id"`
	AccountID            *ids.AccountID `db:"account_id"` // NULL for an unmatched deposit
	Amount               money.Amount   `db:"amount_minor"`
	Currency             string         `db:"currency"`
	Network              string         `db:"network"`
	NetworkTransactionID string         `db:"network_transaction_id"`
	Status               string         `db:"status"`
	NotificationCount    int            `db:"notification_count"`
	CreatedAt            time.Time      `db:"created_at"`
	UpdatedAt            time.Time      `db:"updated_at"`
	Inserted             bool           `db:"inserted"`
}

// When nothing was recorded, a read of the stored deposit gives the details to compare (conflict,
// or, if none differs, our inconsistency). The decision not to record was already made, atomically,
// by the statement above; this read only classifies it.
//
// Concurrency: every notification is inserted (matched or not), so ON CONFLICT always decides. If
// another transaction is inserting the same network ID, Postgres waits for it and checks against
// the committed row, so the outcome is exact (same details -> duplicate, different -> conflict,
// exactly one caller creates), and the read below always finds the conflicting row: it is
// committed, and deposits are never deleted. Under READ COMMITTED the read, a new statement, sees
// it.
const storedDetailsSQL = `
SELECT id, creditor_iban, amount_minor, currency
FROM deposits
WHERE network = $1 AND network_transaction_id = $2
`

const recordConflictSQL = `
INSERT INTO deposit_notification_conflicts (deposit_id, creditor_iban, provider_account_id,
                                            amount_minor, currency, differing_fields)
VALUES ($1, $2, $3, $4, $5, $6)
`

// RecordNotification records a notification in the caller's transaction: the upsert, then, only
// when it created a matched deposit, the deposit.created event in the outbox (unmatched and
// duplicate notifications store no event), or, for a conflict, the conflict record. A Tx, because
// each must commit with the upsert.
func RecordNotification(ctx context.Context, tx db.Tx, n Notification) (Result, error) {
	rows, err := tx.Query(ctx, recordNotificationSQL, ids.New[ids.Deposit](), n.CreditorIBAN,
		n.ProviderAccountID, n.Amount, n.Currency, n.Network, n.NetworkTransactionID)
	if err != nil {
		return Result{}, fmt.Errorf("record notification %s: %w", n.NetworkTransactionID, err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[upsertRow])
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("record notification %s: %w", n.NetworkTransactionID, err)
	}
	if err == nil {
		return recorded(ctx, tx, row)
	}

	stored, found, err := storedDetails(ctx, tx, n)
	if err != nil {
		return Result{}, err
	}
	if !found {
		// ON CONFLICT found a committed deposit with this network ID, and deposits are never deleted.
		return Result{}, fmt.Errorf("notification %s was refused, but no deposit holds it",
			n.NetworkTransactionID)
	}
	differing := DifferingFields(stored.Details, n.details())
	if len(differing) == 0 {
		return Result{Kind: Inconsistent, Deposit: stored.ID}, nil
	}
	// As text: pgx encodes a []string as text[], but not a slice of the named type ComparedField.
	names := make([]string, len(differing))
	for i, f := range differing {
		names[i] = string(f)
	}
	_, err = tx.Exec(ctx, recordConflictSQL, stored.ID, n.CreditorIBAN, n.ProviderAccountID, n.Amount,
		n.Currency, names)
	if err != nil {
		return Result{}, fmt.Errorf("record conflict for %s: %w", n.NetworkTransactionID, err)
	}
	return Result{Kind: Conflict, Deposit: stored.ID, DifferingFields: differing}, nil
}

// recorded classifies the upsert's row: an unmatched deposit is new (suspense) or a duplicate; a
// matched one is a duplicate, or new, with its event to store.
//
// upsertRow is a second mapping of the deposits row (deposits.scanDeposit reads a fixed column list
// by position; the upsert returns new.* and inserted, read by name), so it applies the same checks.
func recorded(ctx context.Context, tx db.Tx, row upsertRow) (Result, error) {
	if err := deposits.CheckCurrencyAndNetwork(row.ID, row.Currency, row.Network); err != nil {
		return Result{}, err
	}
	status, err := deposits.ParseStatus(row.Status)
	if err != nil {
		return Result{}, err
	}
	if !row.Inserted {
		return Result{
			Kind:              Duplicate,
			Deposit:           row.ID,
			Status:            status,
			NotificationCount: row.NotificationCount,
		}, nil
	}
	if status == deposits.StatusUnmatched {
		return Result{Kind: Unmatched, Deposit: row.ID}, nil
	}
	if row.AccountID == nil {
		return Result{}, fmt.Errorf("deposit %v is %s without an account", row.ID, status)
	}
	event, err := enqueueDepositCreated(ctx, tx, row, *row.AccountID)
	if err != nil {
		return Result{}, err
	}
	return Result{Kind: Created, Deposit: row.ID, Event: event}, nil
}

// enqueueDepositCreated stores deposit.created in the outbox, in the transaction that created the
// deposit, and returns the event's ID (the outbox row's ID). The deposit and its event commit
// together or not at all; the relay publishes it.
func enqueueDepositCreated(
	ctx context.Context,
	tx db.Tx,
	row upsertRow,
	account ids.AccountID,
) (ids.EventID, error) {
	id := ids.New[ids.Event]()
	err := outbox.Enqueue(ctx, tx, outbox.NewEvent{
		ID:      id,
		Entity:  outbox.Deposit(row.ID),
		Subject: events.SubjectDepositCreated,
		MsgID:   events.DepositCreatedMessageID(row.ID),
		Payload: events.DepositCreatedV1{
			EventID:              id,
			EventType:            events.SubjectDepositCreated,
			SchemaVersion:        1,
			DepositID:            row.ID,
			Network:              row.Network,
			NetworkTransactionID: row.NetworkTransactionID,
			AccountID:            account,
			AmountMinor:          row.Amount,
			Currency:             row.Currency,
			OccurredAt:           timestamp.Time(row.CreatedAt), // when the deposit entered created
		},
	})
	return id, err
}

type storedDeposit struct {
	ID ids.DepositID
	Details
}

func storedDetails(ctx context.Context, q db.Querier, n Notification) (storedDeposit, bool, error) {
	var stored storedDeposit
	var iban string
	err := q.QueryRow(ctx, storedDetailsSQL, n.Network, n.NetworkTransactionID).
		Scan(&stored.ID, &iban, &stored.Amount, &stored.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedDeposit{}, false, nil
	}
	if err != nil {
		return storedDeposit{}, false,
			fmt.Errorf("stored deposit for %s: %w", n.NetworkTransactionID, err)
	}
	stored.CreditorIBAN = sepa.IBAN(iban) // stored only after sepa.ParseIBAN
	return stored, true, nil
}
