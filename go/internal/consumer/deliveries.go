// Package consumer is what every process that consumes a durable JetStream consumer shares (the
// deposit-worker, later the submitter): the delivery log, the delivery lifecycle that settles each
// message exactly once, the pull loop, and the process runner. Each process supplies only its
// event's decoding and its handler. Node: node/src/consumer/.
//
// The orchestration layer (decision 57), beneath the processes.
package consumer

import (
	"context"
	"fmt"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// The delivery log: one row per message a consumer receives, written before processing and
// completed after. Shared by every consumer, each row naming its consumer (slice 2 decision 36).
// A log, not a guard: repeated deliveries of the same event get their own rows (no unique
// constraint), which is how duplicates are counted. Idempotency comes from the entity's state and
// the ledger, never from this table. Node: consumer/deliveries.ts.

// Name is a consumer that writes to the delivery log: the column's CHECK allows exactly these, and
// they are the durable consumers' names (nats/consumers/).
type Name string

const (
	DepositWorker Name = "deposit-worker"
	Submitter     Name = "submitter"
)

// Outcome is how a delivery ended.
type Outcome string

const (
	// Applied: done, and it changed something.
	Applied Outcome = "applied"
	// Duplicate: done, and it had already been applied.
	Duplicate Outcome = "duplicate"
	// Invalid: never deliver again (redelivering can't fix the message).
	Invalid Outcome = "invalid"
	// Failed: retried later, or (a bug) never delivered again.
	Failed Outcome = "failed"
)

// DeliveryID is a delivery row's bigserial ID (an internal log, never exposed).
type DeliveryID int64

// NewDelivery is a delivery to record. EventID is nil only when the message is too broken to read
// one; NATSMsgID is "" when the message has no Nats-Msg-Id (stored as NULL, as Node's null).
type NewDelivery struct {
	Consumer        Name
	EventID         *ids.EventID
	NATSMsgID       string
	DeliveryAttempt int
}

const startSQL = `
INSERT INTO event_deliveries (consumer, event_id, nats_msg_id, delivery_attempt, outcome)
VALUES ($1, $2, $3, $4, 'in_progress')
RETURNING id
`

// StartDelivery records that a message arrived. Its own small statement (autocommitted, so a
// Querier: the pool), so the row exists even if processing then crashes: a row left in_progress is
// the trace of that crash.
func StartDelivery(ctx context.Context, q db.Querier, d NewDelivery) (DeliveryID, error) {
	var msgID *string // NULL for none
	if d.NATSMsgID != "" {
		msgID = &d.NATSMsgID
	}
	var id DeliveryID
	err := q.QueryRow(ctx, startSQL, d.Consumer, d.EventID, msgID, d.DeliveryAttempt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("start delivery: %w", err)
	}
	return id, nil
}

// Only an in_progress row can be completed, and only once.
const finishSQL = `
UPDATE event_deliveries
SET outcome = $2, completed_at = now()
WHERE id = $1 AND outcome = 'in_progress'
`

// FinishDelivery records a delivery's outcome. An error if the row isn't in progress (already
// finished, or never started).
func FinishDelivery(ctx context.Context, q db.Querier, id DeliveryID, outcome Outcome) error {
	tag, err := q.Exec(ctx, finishSQL, id, outcome)
	if err != nil {
		return fmt.Errorf("finish delivery %d: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("delivery %d is not in progress; outcome %s not recorded", id, outcome)
	}
	return nil
}
