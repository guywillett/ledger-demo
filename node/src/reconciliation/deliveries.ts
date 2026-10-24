import type { Queryable } from '../shared/db.ts';
import type { CheckScope } from './scope.ts';

// Delivery statistics: the consumers' delivery log (whole database: deliveries aren't linked to
// accounts) and repeat notifications from the provider.

// Per consumer (the worker, the submitter: one delivery log for both, slice 2 decision 36).
export type DeliveryStats = {
  // Outcome counts, e.g. { worker: { applied: 12, duplicate: 1 }, submitter: { applied: 3 } }.
  readonly byConsumer: Readonly<Record<string, Readonly<Record<string, number>>>>;
  // Deliveries still in_progress after the stuck threshold: the trace a crash leaves (the
  // redelivery gets its own row), or a consumer still working on it. Consumers with none are
  // absent.
  readonly staleInProgress: Readonly<Record<string, number>>;
  // Repeat notifications from the provider with the same details (notification_count - 1).
  readonly duplicateNotifications: bigint;
};

const DELIVERIES_BY_OUTCOME_SQL = `
SELECT consumer, outcome, count(*)::int AS n
FROM event_deliveries
GROUP BY consumer, outcome
ORDER BY consumer, outcome
`;

const STALE_IN_PROGRESS_SQL = `
SELECT consumer, count(*)::int AS n
FROM event_deliveries
WHERE outcome = 'in_progress' AND received_at <= now() - $1 * interval '1 millisecond'
GROUP BY consumer
ORDER BY consumer
`;

const DUPLICATE_NOTIFICATIONS_SQL = `
SELECT COALESCE(sum(notification_count - 1), 0)::text AS n
FROM incoming_payments
WHERE $1::uuid[] IS NULL OR account_id = ANY($1)
`;

export async function checkDeliveries(db: Queryable, scope: CheckScope): Promise<DeliveryStats> {
  const byOutcome = await db.query<{ consumer: string; outcome: string; n: number }>(
    DELIVERIES_BY_OUTCOME_SQL,
  );
  const byConsumer: Record<string, Record<string, number>> = {};
  for (const row of byOutcome.rows) {
    (byConsumer[row.consumer] ??= {})[row.outcome] = row.n;
  }
  const stale = await db.query<{ consumer: string; n: number }>(STALE_IN_PROGRESS_SQL, [
    scope.stuckAfterMs,
  ]);
  const duplicates = await db.query<{ n: string }>(DUPLICATE_NOTIFICATIONS_SQL, [scope.accounts]);
  return {
    byConsumer,
    staleInProgress: Object.fromEntries(stale.rows.map((r) => [r.consumer, r.n])),
    duplicateNotifications: BigInt(duplicates.rows[0]?.n ?? '0'),
  };
}
