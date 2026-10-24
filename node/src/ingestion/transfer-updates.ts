import { execute, release } from '../ledger/withdrawals.ts';
import type { Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import { TransferIdMismatchError, type WithdrawalStatus } from '../withdrawals/status.ts';
import { getWithdrawal } from '../withdrawals/withdrawals.ts';

// Ingestion's second kind of provider notification (the first is incoming-payments.ts): the
// provider's result for a transfer we sent. Recorded, then settled, in the caller's transaction
// (slice 2 decision 29): executed -> held to external, failed -> funds released.
//
// The money is decided by the withdrawal's conditional transition, never by the record: the
// transfer_updates row only recognises a resent update and keeps contradictions and unmatched
// updates on file. Those are committed, not rolled back, so they stay visible; they move no
// money (decision 35). Only invariant violations (ledger bugs) propagate and roll back.

export type TransferUpdate = {
  readonly providerEventId: string;
  readonly transferId: string;
  // As the provider sent it.
  readonly reference: string;
  // The withdrawal the reference names, when it is a wd_ TypeID; null otherwise.
  readonly withdrawalId: Uuid<'wd'> | null;
  readonly status: 'executed' | 'failed';
  // Set exactly for failed.
  readonly reason: string | null;
  readonly occurredAt: Date;
};

type Final = 'executed' | 'failed';

export type UpdateResult =
  // A new update, and it settled the withdrawal.
  | { readonly kind: 'applied'; readonly withdrawalId: Uuid<'wd'> }
  // Nothing settled again: the same event resent (notificationCount > 1), or a second event for
  // the result already recorded.
  | {
      readonly kind: 'duplicate';
      readonly withdrawalId: Uuid<'wd'>;
      readonly notificationCount: number;
    }
  // A contradiction: the withdrawal already has the other final status, or another transfer ID.
  // Recorded and moving no money.
  | {
      readonly kind: 'conflict';
      readonly withdrawalId: Uuid<'wd'>;
      readonly stored: { readonly status: WithdrawalStatus; readonly transferId: string | null };
      readonly reported: { readonly status: Final; readonly transferId: string };
    }
  // The provider reused one of its event IDs for different details (its bug). Nothing changed.
  | { readonly kind: 'event_conflict' }
  // The reference names no withdrawal: recorded, nothing to settle. Not a timing race (a
  // withdrawal exists before it is submitted), so a misroute or the provider's bug.
  | { readonly kind: 'unmatched'; readonly reference: string };

// One statement records the update, with the withdrawal resolved from the reference, or
// recognises a resend (same event ID, same details: count it). The outcome stored here is
// provisional ('applied', or 'unmatched' when unresolved); settling sets the final one.
//
// IS NOT DISTINCT FROM, not =: reason is NULL for an executed update, and NULL = NULL is not
// true in SQL, which would make every resent executed update look like an event-ID conflict.
// Returns no row when the event ID is stored with other details.
const RECORD_SQL = `
INSERT INTO transfer_updates (provider_event_id, withdrawal_id, reference, transfer_id, status,
                              reason, occurred_at, outcome)
SELECT $1, w.id, $3, $4, $5, $6, $7, CASE WHEN w.id IS NULL THEN 'unmatched' ELSE 'applied' END
FROM (SELECT $2::uuid AS id) ref
LEFT JOIN withdrawals w ON w.id = ref.id
ON CONFLICT (provider_event_id) DO UPDATE
  SET notification_count = transfer_updates.notification_count + 1
  WHERE (transfer_updates.reference, transfer_updates.transfer_id, transfer_updates.status,
         transfer_updates.reason)
        IS NOT DISTINCT FROM (EXCLUDED.reference, EXCLUDED.transfer_id, EXCLUDED.status,
                              EXCLUDED.reason)
RETURNING id, withdrawal_id, notification_count, (old.id IS NULL) AS inserted
`;

const SET_OUTCOME_SQL = 'UPDATE transfer_updates SET outcome = $2 WHERE id = $1';

export async function recordTransferUpdate(tx: Tx, u: TransferUpdate): Promise<UpdateResult> {
  // 1. Record.
  const recorded = await tx.query<{
    id: bigint;
    withdrawal_id: string | null;
    notification_count: number;
    inserted: boolean;
  }>(RECORD_SQL, [
    u.providerEventId,
    u.withdrawalId,
    u.reference,
    u.transferId,
    u.status,
    u.reason,
    u.occurredAt,
  ]);
  const row = recorded.rows[0];
  if (row === undefined) return { kind: 'event_conflict' };
  if (row.withdrawal_id === null) {
    // Unmatched, whether new or resent: nothing to settle.
    return { kind: 'unmatched', reference: u.reference };
  }
  const withdrawalId = row.withdrawal_id as Uuid<'wd'>;
  if (!row.inserted) {
    // The same event resent: its first receipt already settled it, atomically.
    return { kind: 'duplicate', withdrawalId, notificationCount: row.notification_count };
  }

  // 2. Settle a new, matched update.
  const result = await settle(tx, withdrawalId, u);

  // 3. The final outcome (the provisional one was 'applied').
  if (result.kind !== 'applied') await tx.query(SET_OUTCOME_SQL, [row.id, result.kind]);
  return result;
}

async function settle(tx: Tx, withdrawalId: Uuid<'wd'>, u: TransferUpdate): Promise<UpdateResult> {
  const reported = { status: u.status, transferId: u.transferId };
  let settled: 'applied' | 'skipped';
  try {
    settled =
      u.status === 'executed'
        ? await execute(tx, withdrawalId, u.transferId)
        : await release(tx, withdrawalId, u.reason ?? 'failed', u.transferId);
  } catch (err) {
    // Another transfer than the one stored. Thrown after a read (the transition's UPDATE matched
    // nothing), not by a failed statement, so the transaction is still usable: record it.
    if (!(err instanceof TransferIdMismatchError)) throw err;
    const stored = await storedState(tx, withdrawalId);
    return { kind: 'conflict', withdrawalId, stored, reported };
  }
  if (settled === 'applied') return { kind: 'applied', withdrawalId };

  // Skipped: already final. This read only classifies; the transition has decided.
  const stored = await storedState(tx, withdrawalId);
  if (stored.status === u.status) {
    // A second event for the same result.
    return { kind: 'duplicate', withdrawalId, notificationCount: 1 };
  }
  // The other final status: executed then failed, or the reverse, which SEPA doesn't allow
  // (a return after execution is a separate flow, out of scope).
  return { kind: 'conflict', withdrawalId, stored, reported };
}

async function storedState(
  tx: Tx,
  withdrawalId: Uuid<'wd'>,
): Promise<{ status: WithdrawalStatus; transferId: string | null }> {
  const withdrawal = await getWithdrawal(tx, withdrawalId);
  if (withdrawal === undefined) {
    // The update's row references it (a foreign key), and withdrawals are never deleted.
    throw new Error(`withdrawal ${withdrawalId} disappeared`);
  }
  return { status: withdrawal.status, transferId: withdrawal.providerTransferId };
}
