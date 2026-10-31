// Package outbox is the transactional outbox (slice 2, Part A): every module that emits events
// writes them here, in the same transaction as the state change they announce, and the relay
// publishes them. Nothing else publishes events. Node: node/src/outbox/outbox.ts. Claiming and
// marking come with the relay (step 5).
//
// The entity layer (decision 57): the outbox table is an entity like deposits and withdrawals.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// Entity is what an event is about. Its fields are unexported, so an Entity comes only from
// Deposit or Withdrawal, each taking its own ID type: a deposit event can't carry a withdrawal's
// ID (Node: a discriminated union, OutboxEntity). The zero Entity is rejected by Enqueue.
type Entity struct {
	entityType string    // entity_type
	id         uuid.UUID // entity_id
}

// Deposit is a deposit's event.
func Deposit(id ids.DepositID) Entity {
	return Entity{entityType: "deposit", id: id.UUID()}
}

// Withdrawal is a withdrawal's event.
func Withdrawal(id ids.WithdrawalID) Entity {
	return Entity{entityType: "withdrawal", id: id.UUID()}
}

// NewEvent is an event to store.
type NewEvent struct {
	// The event's event_id: the row's primary key, so it never changes however often the event
	// is published.
	ID     ids.EventID
	Entity Entity
	// The subject it is published on (events.Subject…).
	Subject string
	// The Nats-Msg-Id: "<entity TypeID>.<status>".
	MsgID string
	// The event in its wire format (an events struct), written as JSON.
	Payload any
}

// The payload goes in as JSON text cast to jsonb, explicitly, as in Node.
const enqueueSQL = `
INSERT INTO outbox (id, entity_type, entity_id, subject, msg_id, payload)
VALUES ($1, $2, $3, $4, $5, $6::jsonb)
ON CONFLICT (msg_id) DO NOTHING
`

// Enqueue stores an event in the caller's transaction: it commits or rolls back with the state
// change. A Tx, because that shared transaction is the whole point.
//
// A msg_id already stored means a second event for the same entity and status, which the
// conditional transitions should make impossible: a bug, not a retry, so it is an error (and the
// caller's transaction rolls back).
func Enqueue(ctx context.Context, tx db.Tx, e NewEvent) error {
	if e.Entity.entityType == "" {
		return fmt.Errorf("outbox event %s without an entity", e.MsgID)
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("outbox event %s: %w", e.MsgID, err)
	}
	tag, err := tx.Exec(ctx, enqueueSQL,
		e.ID, e.Entity.entityType, e.Entity.id, e.Subject, e.MsgID, string(payload))
	if err != nil {
		return fmt.Errorf("outbox event %s: %w", e.MsgID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("outbox already holds an event with msg_id %s", e.MsgID)
	}
	return nil
}
