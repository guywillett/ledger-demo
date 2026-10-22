import type { Queryable, Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';

// The transactional outbox (slice 2, Part A): every module that emits events writes them here,
// in the same transaction as the state change they announce, and the relay (src/relay/)
// publishes them. Nothing else publishes events.

// What an event is about. A discriminated union, like move()'s MovementReference: entityType
// decides which entity's UUID entityId is, so a payment event can't carry a withdrawal's ID.
export type OutboxEntity =
  | { readonly entityType: 'payment'; readonly entityId: Uuid<'pay'> }
  | { readonly entityType: 'withdrawal'; readonly entityId: Uuid<'wd'> };
export type EntityType = OutboxEntity['entityType'];

export type NewOutboxEvent = {
  // The event's event_id: the row's primary key, so it never changes however often the event
  // is published.
  readonly id: Uuid<'evt'>;
  readonly subject: string;
  // The Nats-Msg-Id: <entity TypeID>.<status>.
  readonly msgId: string;
  // The event in its wire format (strings, TypeIDs), as built by the event's builder.
  readonly payload: object;
} & OutboxEntity;

// A claimed event, ready to publish.
export type OutboxEvent = {
  readonly id: Uuid<'evt'>;
  readonly subject: string;
  readonly msgId: string;
  readonly payload: object;
  // Including this claim: 1 on the first attempt.
  readonly attempts: number;
} & OutboxEntity;

// The payload goes in as JSON text cast to jsonb, explicitly. pg would serialise a plain object
// to JSON itself, but an array would become a Postgres array literal; JSON.stringify is
// unambiguous whatever the payload.
const ENQUEUE_SQL = `
INSERT INTO outbox (id, entity_type, entity_id, subject, msg_id, payload)
VALUES ($1, $2, $3, $4, $5, $6::jsonb)
ON CONFLICT (msg_id) DO NOTHING
`;

// Stores an event in the caller's transaction: it commits or rolls back with the state change.
// Takes a Tx because that shared transaction is the whole point.
//
// A msg_id already stored means a second event for the same entity and status, which the
// conditional transitions should make impossible: a bug, not a retry, so it throws (and the
// caller's transaction rolls back).
export async function enqueue(tx: Tx, e: NewOutboxEvent): Promise<void> {
  const result = await tx.query(ENQUEUE_SQL, [
    e.id,
    e.entityType,
    e.entityId,
    e.subject,
    e.msgId,
    JSON.stringify(e.payload),
  ]);
  if (result.rowCount !== 1) {
    throw new Error(`outbox already holds an event with msg_id ${e.msgId}`);
  }
}

// Claims up to $1 unpublished events whose lease has expired (or that were never claimed),
// oldest first, leasing them for $2 ms and counting the attempt. One statement, so it is atomic
// on its own: no transaction needed.
//
// FOR UPDATE SKIP LOCKED (Django: select_for_update(skip_locked=True)): the subquery locks the
// rows it picks, and *skips* rows another transaction has locked instead of waiting for them.
// Two relays claiming at the same moment therefore get disjoint batches without blocking each
// other: each takes the oldest rows the other hasn't. Without SKIP LOCKED, the second would
// wait for the first to commit, then find those rows leased and pick none of them; with NOWAIT
// it would fail instead. Once the claim commits, the lease (claimed_until) keeps the rows out of
// other claims until it expires, while the relay publishes them outside any transaction.
//
// UPDATE … RETURNING returns rows in no particular order, hence the outer ORDER BY.
const CLAIM_SQL = `
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
`;

export async function claim(
  db: Queryable,
  opts: { limit: number; leaseMs: number },
): Promise<OutboxEvent[]> {
  const result = await db.query<{
    id: string;
    entity_type: string;
    entity_id: string;
    subject: string;
    msg_id: string;
    // pg parses jsonb into a JavaScript value.
    payload: object;
    attempts: number;
  }>(CLAIM_SQL, [opts.limit, opts.leaseMs]);
  return result.rows.map((row) => ({
    id: row.id as Uuid<'evt'>,
    ...entityFromRow(row.entity_type, row.entity_id),
    subject: row.subject,
    msgId: row.msg_id,
    payload: row.payload,
    attempts: row.attempts,
  }));
}

// The column's CHECK allows only these.
function entityFromRow(type: string, id: string): OutboxEntity {
  switch (type) {
    case 'payment':
      return { entityType: 'payment', entityId: id as Uuid<'pay'> };
    case 'withdrawal':
      return { entityType: 'withdrawal', entityId: id as Uuid<'wd'> };
    default:
      throw new Error(`unexpected outbox entity_type in database: ${type}`);
  }
}

const MARK_PUBLISHED_SQL = `
UPDATE outbox
SET published_at = now(), claimed_until = NULL, last_error = NULL
WHERE id = $1 AND published_at IS NULL
`;

// After the stream's acknowledgement. 'skipped': already published, e.g. by another relay that
// reclaimed the event after this one's lease ran out (the stream dropped one of the two
// publishes as a duplicate).
export async function markPublished(
  db: Queryable,
  id: Uuid<'evt'>,
): Promise<'applied' | 'skipped'> {
  const result = await db.query(MARK_PUBLISHED_SQL, [id]);
  return result.rowCount === 1 ? 'applied' : 'skipped';
}

// The lease moves to now() + retryInMs: the event is retried after that (by any relay), rather
// than reclaimed at once and failing again in a tight loop.
const MARK_FAILED_SQL = `
UPDATE outbox
SET last_error = $2, claimed_until = now() + $3 * interval '1 millisecond'
WHERE id = $1 AND published_at IS NULL
`;

// 'skipped': published meanwhile by another relay, so there is nothing to retry.
export async function markFailed(
  db: Queryable,
  id: Uuid<'evt'>,
  error: string,
  retryInMs: number,
): Promise<'applied' | 'skipped'> {
  const result = await db.query(MARK_FAILED_SQL, [id, error, retryInMs]);
  return result.rowCount === 1 ? 'applied' : 'skipped';
}
