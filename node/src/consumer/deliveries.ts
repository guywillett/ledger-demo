import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';

// The delivery log: one row per message a consumer receives, written before processing and
// completed after. Shared by every consumer (the worker, the submitter), each row naming its
// consumer (slice 2 decision 36). A log, not a guard: repeated deliveries of the same event get
// their own rows (no unique constraint), which is how duplicates are counted. Idempotency comes
// from the entity's state and the ledger, never from this table.

// The consumers that write here; the column's CHECK allows exactly these.
export type ConsumerName = 'worker' | 'submitter';

export type DeliveryOutcome = 'applied' | 'duplicate' | 'invalid' | 'failed';

// The row's bigserial ID (an internal log, never exposed).
export type DeliveryId = bigint & { readonly __brand: 'DeliveryId' };

const START_SQL = `
INSERT INTO event_deliveries (consumer, event_id, nats_msg_id, delivery_attempt, outcome)
VALUES ($1, $2, $3, $4, 'in_progress')
RETURNING id
`;

// Only an in_progress row can be completed, and only once.
const FINISH_SQL = `
UPDATE event_deliveries
SET outcome = $2, completed_at = now()
WHERE id = $1 AND outcome = 'in_progress'
`;

// Records that a message arrived. Its own small statement (autocommitted), so the row exists
// even if processing then crashes: a row left in_progress is the trace of that crash.
// eventId is null only when the message is too broken to read one.
export async function startDelivery(
  db: Queryable,
  d: {
    consumer: ConsumerName;
    eventId: Uuid<'evt'> | null;
    natsMsgId: string | null;
    deliveryAttempt: number;
  },
): Promise<DeliveryId> {
  const result = await db.query<{ id: bigint }>(START_SQL, [
    d.consumer,
    d.eventId,
    d.natsMsgId,
    d.deliveryAttempt,
  ]);
  const row = result.rows[0];
  if (row === undefined) throw new Error('startDelivery: no row returned');
  return row.id as DeliveryId;
}

export async function finishDelivery(
  db: Queryable,
  id: DeliveryId,
  outcome: DeliveryOutcome,
): Promise<void> {
  const result = await db.query(FINISH_SQL, [id, outcome]);
  if (result.rowCount !== 1) {
    throw new Error(`delivery ${id} is not in progress; outcome ${outcome} not recorded`);
  }
}
