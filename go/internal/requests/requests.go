// Package requests is client withdrawal requests: it turns a request into a result, so the API
// handler stays thin. The client-facing counterpart of ingestion (provider notifications). Node:
// node/src/withdrawals/requests.ts and events.ts.
//
// The orchestration layer (decision 57): it calls the ledger (CreateWithdrawal, which holds the
// funds) and stores the withdrawal.held event.
//
// Idempotency (slice 2 decision 24): the key is a unique column on the withdrawal itself, so only
// a request that commits takes it. Everything that fails rolls back and leaves it free:
// insufficient funds (*ledger.InsufficientFundsError) and an unknown account
// (*withdrawals.AccountNotFoundError) are returned as errors, for the caller to handle once
// db.InTx has rolled back.
package requests

import (
	"context"
	"fmt"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/outbox"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// ComparedField is a detail that must match for a repeat with the same key to count as the same
// request; the wire names, as the 409's differing_fields lists them.
type ComparedField string

const (
	FieldAmount         ComparedField = "amount_minor"
	FieldCurrency       ComparedField = "currency"
	FieldCreditorIBAN   ComparedField = "creditor_iban"
	FieldCreditorName   ComparedField = "creditor_name"
	FieldRemittanceInfo ComparedField = "remittance_info"
)

// Details are the compared details, as both a stored withdrawal and a request have them.
type Details struct {
	Amount         money.Amount
	Currency       string
	CreditorIBAN   sepa.IBAN
	CreditorName   string
	RemittanceInfo *string // nil for none
}

// requestCurrency is a request's currency: the only one, which the API's validation requires
// (Node's z.literal('EUR')); NewWithdrawal doesn't carry it, since the column's default and CHECK
// make every withdrawal EUR.
const requestCurrency = "EUR"

// DetailsOf are a stored withdrawal's details.
func DetailsOf(w withdrawals.Withdrawal) Details {
	return Details{
		Amount:         w.Amount,
		Currency:       w.Currency,
		CreditorIBAN:   w.CreditorIBAN,
		CreditorName:   w.CreditorName,
		RemittanceInfo: w.RemittanceInfo,
	}
}

// RequestDetails are a request's details.
func RequestDetails(w withdrawals.NewWithdrawal) Details {
	return Details{
		Amount:         w.Amount,
		Currency:       requestCurrency,
		CreditorIBAN:   w.CreditorIBAN,
		CreditorName:   w.CreditorName,
		RemittanceInfo: w.RemittanceInfo,
	}
}

// DifferingFields lists the fields in which a repeat with the same key differs from the stored
// withdrawal (empty: the same request). Field by field, no request hash (slice 1 decision 16), so
// Node and Go share one rule; contracts/vectors/withdrawal-same-details.json pins it for both.
func DifferingFields(stored, request Details) []ComparedField {
	var fields []ComparedField
	if stored.Amount != request.Amount {
		fields = append(fields, FieldAmount)
	}
	if stored.Currency != request.Currency {
		fields = append(fields, FieldCurrency)
	}
	if stored.CreditorIBAN != request.CreditorIBAN {
		fields = append(fields, FieldCreditorIBAN)
	}
	if stored.CreditorName != request.CreditorName {
		fields = append(fields, FieldCreditorName)
	}
	if !sameOptional(stored.RemittanceInfo, request.RemittanceInfo) {
		fields = append(fields, FieldRemittanceInfo)
	}
	return fields
}

// sameOptional compares two optional texts by value (nil only equals nil), as Node's !== compares
// two strings or two nulls.
func sameOptional(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Kind is what a request did.
type Kind int

const (
	// Created: a new withdrawal, its funds held, its withdrawal.held event stored.
	Created Kind = iota + 1
	// Repeat: the same request again; Withdrawal is it in its current status (not a copy of the
	// first response).
	Repeat
	// Conflict: the key was used for another request; Withdrawal is that one, DifferingFields how
	// this request differs.
	Conflict
)

// Result is what RequestWithdrawal did (Node: RequestResult, a union), one struct with a Kind as
// ingestion.Result: the fields a Kind doesn't use are zero.
type Result struct {
	Kind            Kind
	Withdrawal      withdrawals.Withdrawal
	Event           ids.EventID // Created: the withdrawal.held event stored with it
	DifferingFields []ComparedField
}

// RequestWithdrawal creates the withdrawal with its funds held and stores its withdrawal.held
// event, in the caller's transaction, or, if the account has already used the key, classifies the
// repeat. A Tx: the withdrawal, its hold and its event commit together.
//
// The read of the stored withdrawal only classifies: the insert's ON CONFLICT has already decided
// not to insert, and the row it conflicted with is committed (ON CONFLICT waited for any
// transaction still inserting it). Under READ COMMITTED the read, a new statement, sees it.
func RequestWithdrawal(ctx context.Context, tx db.Tx, w withdrawals.NewWithdrawal) (Result, error) {
	created, inserted, err := ledger.CreateWithdrawal(ctx, tx, w)
	if err != nil {
		return Result{}, err
	}
	if inserted {
		event, err := enqueueWithdrawalHeld(ctx, tx, created)
		if err != nil {
			return Result{}, err
		}
		return Result{Kind: Created, Withdrawal: created, Event: event}, nil
	}

	stored, found, err := withdrawals.GetByKey(ctx, tx, w.Account, w.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	if !found {
		// ON CONFLICT found a committed row with this key, and withdrawals are never deleted.
		return Result{}, fmt.Errorf(
			"withdrawal key conflict on account %v, but no withdrawal holds the key", w.Account)
	}
	differing := DifferingFields(DetailsOf(stored), RequestDetails(w))
	if len(differing) == 0 {
		return Result{Kind: Repeat, Withdrawal: stored}, nil
	}
	return Result{Kind: Conflict, Withdrawal: stored, DifferingFields: differing}, nil
}

// enqueueWithdrawalHeld stores withdrawal.held in the outbox, in the transaction that created the
// withdrawal, and returns the event's ID. IDs, the amount and the currency only: the IBAN, the
// creditor's name and the remittance text are personal data, and a stream keeps messages beyond
// the database's control; consumers read them from the database by withdrawal_id.
func enqueueWithdrawalHeld(
	ctx context.Context,
	tx db.Tx,
	w withdrawals.Withdrawal,
) (ids.EventID, error) {
	id := ids.New[ids.Event]()
	err := outbox.Enqueue(ctx, tx, outbox.NewEvent{
		ID:      id,
		Entity:  outbox.Withdrawal(w.ID),
		Subject: events.SubjectWithdrawalHeld,
		MsgID:   events.WithdrawalMessageID(w.ID, string(withdrawals.StatusHeld)),
		Payload: events.WithdrawalHeldV1{
			EventID:       id,
			EventType:     events.SubjectWithdrawalHeld,
			SchemaVersion: 1,
			WithdrawalID:  w.ID,
			AccountID:     w.Account,
			AmountMinor:   w.Amount,
			Currency:      w.Currency,
			// When it entered held: it was created held. Milliseconds, as the response's
			// created_at (timestamp.Time writes them so).
			OccurredAt: timestamp.Time(w.CreatedAt.Truncate(time.Millisecond)),
		},
	})
	return id, err
}
