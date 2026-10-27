import type { Queryable } from '../shared/db.ts';
import type { CheckScope } from './scope.ts';

// The outbox check: events not yet published.

export type OutboxStats = {
  // Events unpublished for longer than the stuck threshold: the relay is down or failing.
  readonly stuckUnpublished: number;
  // The oldest unpublished event's age; null when everything is published.
  readonly oldestUnpublishedAgeSeconds: number | null;
  // The most publish attempts among unpublished events (0 when there are none): a high
  // number means an event keeps failing to publish.
  readonly maxUnpublishedAttempts: number;
};

// Unpublished events, for the accounts checked: those whose entity belongs to one of them.
// min() and max() of no rows are NULL.
const OUTBOX_SQL = `
SELECT count(*) FILTER (WHERE o.created_at <= now() - $2 * interval '1 millisecond')::int AS stuck,
       floor(extract(epoch FROM now() - min(o.created_at)))::int AS oldest_age_seconds,
       COALESCE(max(o.attempts), 0) AS max_attempts
FROM outbox o
WHERE o.published_at IS NULL
  AND ($1::uuid[] IS NULL
       OR EXISTS (
         SELECT 1 FROM deposits p
         WHERE o.entity_type = 'deposit' AND p.id = o.entity_id AND p.account_id = ANY($1))
       OR EXISTS (
         SELECT 1 FROM withdrawals w
         WHERE o.entity_type = 'withdrawal' AND w.id = o.entity_id AND w.account_id = ANY($1)))
`;

export async function checkOutbox(db: Queryable, scope: CheckScope): Promise<OutboxStats> {
  const outbox = await db.query<{
    stuck: number;
    oldest_age_seconds: number | null;
    max_attempts: number;
  }>(OUTBOX_SQL, [scope.accounts, scope.stuckAfterMs]);
  const outboxRow = outbox.rows[0];
  return {
    stuckUnpublished: outboxRow?.stuck ?? 0,
    oldestUnpublishedAgeSeconds: outboxRow?.oldest_age_seconds ?? null,
    maxUnpublishedAttempts: outboxRow?.max_attempts ?? 0,
  };
}
