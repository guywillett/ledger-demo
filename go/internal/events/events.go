// Package events is the events' wire format: their subjects, payloads and message IDs, shared by
// the code that produces them (ingestion; withdrawal requests) and the code that consumes them
// (the deposit-worker, the submitter). Node: node/src/shared/events.ts and the builders in
// ingestion/events.ts. The language-neutral source is contracts/events/*.json; tests check every
// payload against it.
//
// A foundation package (decision 57), like ids and money: it is contract, and depends only on
// other foundations, so producers and consumers at any layer can import it. Events carry our IDs
// (TypeIDs), never provider identifiers such as IBANs.
package events

import (
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// The subjects, one per event type: the event type is the subject.
const (
	SubjectDepositCreated = "deposit.created"
	SubjectWithdrawalHeld = "withdrawal.held"
)

// DepositCreatedV1 is deposit.created, schema version 1 (contracts/events/deposit.created.v1.json):
// a deposit entered status created. The JSON tags are the wire names, in Node's order.
type DepositCreatedV1 struct {
	EventID              ids.EventID    `json:"event_id"`
	EventType            string         `json:"event_type"`
	SchemaVersion        int            `json:"schema_version"`
	DepositID            ids.DepositID  `json:"deposit_id"`
	Network              string         `json:"network"`
	NetworkTransactionID string         `json:"network_transaction_id"`
	AccountID            ids.AccountID  `json:"account_id"`
	AmountMinor          money.Amount   `json:"amount_minor"`
	Currency             string         `json:"currency"`
	OccurredAt           timestamp.Time `json:"occurred_at"` // when the deposit entered created
}

// DepositCreatedMessageID is the Nats-Msg-Id for a deposit's deposit.created: one per deposit and
// event type (not the event_id), "<dep TypeID>.created". If the relay publishes the same outbox row
// twice (it crashed after publishing but before marking it), the stream drops the repeat within
// its duplicate window; beyond the window, consumers' idempotency on deposit state handles it.
func DepositCreatedMessageID(deposit ids.DepositID) string {
	return deposit.TypeID() + ".created"
}
