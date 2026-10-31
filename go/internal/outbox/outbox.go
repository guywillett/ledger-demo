// Package outbox is the transactional outbox (slice 2, Part A): every module that emits events
// writes them here, in the same transaction as the state change they announce, and the relay
// publishes them (claiming, then marking each event published or failed). Nothing else publishes
// events. Node: node/src/outbox/outbox.ts.
//
// The entity layer (decision 57): the outbox table is an entity like deposits and withdrawals.
package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// Entity is what an event is about. Its fields are unexported, so an Entity comes only from
// Deposit or Withdrawal, each taking its own ID type: a deposit event can't carry a withdrawal's
// ID (Node: a discriminated union, OutboxEntity). The zero Entity is rejected by Enqueue.
type Entity struct {
	entityType string // entity_type: which of the two IDs is set
	deposit    ids.DepositID
	withdrawal ids.WithdrawalID
}

// Deposit is a deposit's event.
func Deposit(id ids.DepositID) Entity {
	return Entity{entityType: "deposit", deposit: id}
}

// Withdrawal is a withdrawal's event.
func Withdrawal(id ids.WithdrawalID) Entity {
	return Entity{entityType: "withdrawal", withdrawal: id}
}

// DepositID is the deposit's ID, if the event is about a deposit (comma ok: false otherwise).
func (e Entity) DepositID() (ids.DepositID, bool) {
	return e.deposit, e.entityType == "deposit"
}

// WithdrawalID is the withdrawal's ID, if the event is about a withdrawal.
func (e Entity) WithdrawalID() (ids.WithdrawalID, bool) {
	return e.withdrawal, e.entityType == "withdrawal"
}

// LogAttr is the entity's ID as a log field, deposit_id or withdrawal_id with its TypeID, so a log
// line about an event can be found from the entity (a stuck dep_…) as well as from the event.
func (e Entity) LogAttr() slog.Attr {
	if e.entityType == "withdrawal" {
		return slog.String("withdrawal_id", e.withdrawal.TypeID())
	}
	return slog.String("deposit_id", e.deposit.TypeID())
}

// id is entity_id.
func (e Entity) id() uuid.UUID {
	if e.entityType == "withdrawal" {
		return e.withdrawal.UUID()
	}
	return e.deposit.UUID()
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
		e.ID, e.Entity.entityType, e.Entity.id(), e.Subject, e.MsgID, string(payload))
	if err != nil {
		return fmt.Errorf("outbox event %s: %w", e.MsgID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("outbox already holds an event with msg_id %s", e.MsgID)
	}
	return nil
}

// Event is a claimed event, ready to publish.
type Event struct {
	ID      ids.EventID
	Entity  Entity
	Subject string
	MsgID   string
	// The payload as compact JSON, the bytes to publish (see Claim).
	Payload []byte
	// Including this claim: 1 on the first attempt.
	Attempts int
}

// Claims up to $1 unpublished events whose lease has expired (or that were never claimed), oldest
// first, leasing them for $2 ms and counting the attempt. One statement, so it is atomic on its
// own: no transaction needed.
//
// FOR UPDATE SKIP LOCKED (Django: select_for_update(skip_locked=True)): the subquery locks the rows
// it picks, and skips rows another transaction has locked instead of waiting for them. Two relays
// claiming at the same moment therefore get disjoint batches without blocking each other. Once the
// claim commits, the lease (claimed_until) keeps the rows out of other claims until it expires,
// while the relay publishes them outside any transaction.
//
// UPDATE … RETURNING returns rows in no particular order, hence the outer ORDER BY.
const claimSQL = `
WITH claimed AS (
  UPDATE outbox
  SET claimed_until = now() + $2 * interval '1 millisecond',
      attempts = attempts + 1
  WHERE id IN (
    SELECT id FROM outbox
    WHERE published_at IS NULL
      AND (claimed_until IS NULL OR claimed_until < now())
    ORDER BY id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
  )
  RETURNING id, entity_type, entity_id, subject, msg_id, payload, attempts
)
SELECT * FROM claimed ORDER BY id
`

// Claim claims up to limit events, leased for lease (see claimSQL), in id order. A single
// statement, so a Querier (the pool).
//
// The payload: Postgres returns jsonb as its own text form, with a space after each : and , (and
// its keys in jsonb's storage order, shorter keys first). Node's pg parses that and the relay's
// JSON.stringify writes it back compact, so Node publishes the same keys, in the same order,
// without the spaces.
// json.Compact removes exactly that whitespace, so Go publishes the same bytes. (For the strings
// and small integers our payloads hold, compacting and re-stringifying agree; a JavaScript
// round trip would also rewrite numbers such as 1.0 or beyond 2^53, which events don't carry.)
func Claim(ctx context.Context, q db.Querier, limit int, lease time.Duration) ([]Event, error) {
	rows, err := q.Query(ctx, claimSQL, limit, lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	events, err := pgx.CollectRows(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	return events, nil
}

func scanEvent(row pgx.CollectableRow) (Event, error) {
	var e Event
	var entityType string
	var entityID ids.DepositID // either kind: converted below once entity_type says which
	var payload []byte         // pgx gives jsonb as its JSON text
	err := row.Scan(&e.ID, &entityType, &entityID, &e.Subject, &e.MsgID, &payload, &e.Attempts)
	if err != nil {
		return Event{}, err
	}
	switch entityType { // the column's CHECK allows only these
	case "deposit":
		e.Entity = Deposit(entityID)
	case "withdrawal":
		e.Entity = Withdrawal(ids.WithdrawalID(entityID))
	default:
		return Event{}, fmt.Errorf("unexpected outbox entity_type in database: %s", entityType)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return Event{}, fmt.Errorf("outbox event %v: payload: %w", e.ID, err)
	}
	e.Payload = compact.Bytes()
	return e, nil
}

// Result is what a mark did: Applied, or Skipped (the event was already published).
type Result string

const (
	Applied Result = "applied"
	Skipped Result = "skipped"
)

func resultOf(rowsAffected int64) Result {
	if rowsAffected == 1 {
		return Applied
	}
	return Skipped
}

const markPublishedSQL = `
UPDATE outbox
SET published_at = now(), claimed_until = NULL, last_error = NULL
WHERE id = $1 AND published_at IS NULL
`

// MarkPublished records the stream's acknowledgement. Skipped: already published, e.g. by another
// relay that reclaimed the event after this one's lease ran out (the stream dropped one of the two
// publishes as a duplicate).
func MarkPublished(ctx context.Context, q db.Querier, id ids.EventID) (Result, error) {
	tag, err := q.Exec(ctx, markPublishedSQL, id)
	if err != nil {
		return "", fmt.Errorf("mark outbox event %v published: %w", id, err)
	}
	return resultOf(tag.RowsAffected()), nil
}

// The lease moves to now() + retryIn: the event is retried after that (by any relay), rather than
// reclaimed at once and failing again in a tight loop.
const markFailedSQL = `
UPDATE outbox
SET last_error = $2, claimed_until = now() + $3 * interval '1 millisecond'
WHERE id = $1 AND published_at IS NULL
`

// MarkFailed records a failed publish, to be retried after retryIn. Skipped: published meanwhile
// by another relay, so there is nothing to retry.
func MarkFailed(
	ctx context.Context,
	q db.Querier,
	id ids.EventID,
	errText string,
	retryIn time.Duration,
) (Result, error) {
	tag, err := q.Exec(ctx, markFailedSQL, id, errText, retryIn.Milliseconds())
	if err != nil {
		return "", fmt.Errorf("mark outbox event %v failed: %w", id, err)
	}
	return resultOf(tag.RowsAffected()), nil
}
