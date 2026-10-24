import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import {
  parseWithdrawalStatus,
  UNSETTLED_WITHDRAWAL_STATUSES,
  type WithdrawalStatus,
} from '../withdrawals/status.ts';
import type { CheckScope } from './scope.ts';
import { sameSteps } from './steps.ts';

// The withdrawal checks: each withdrawal's movements against its status (and movements for no
// withdrawal), withdrawals stuck held or submitted, and provider transfer updates a person should
// look at.

// A withdrawal whose movements don't match its status (or movements for no withdrawal), as for
// payments.
export type WithdrawalMismatch = {
  readonly withdrawalId: Uuid<'wd'>;
  readonly accountId: Uuid<'acct'>;
  // null: movements reference a withdrawal that doesn't exist.
  readonly status: WithdrawalStatus | null;
  readonly steps: readonly string[];
  readonly expectedSteps: readonly string[];
  readonly otherAccount: boolean;
  readonly wrongAmount: boolean;
};

// A withdrawal still held or submitted after the threshold: funds reserved, outcome unknown.
export type StuckWithdrawal = {
  readonly withdrawalId: Uuid<'wd'>;
  readonly accountId: Uuid<'acct'>;
  readonly status: WithdrawalStatus;
  readonly since: Date;
  readonly ageSeconds: number;
};

// A provider transfer update a person should look at: a contradiction (the other final status,
// or another transfer ID) or one matching no withdrawal. Recorded and moving no money, so not
// inconsistent books.
export type TransferUpdateProblem = {
  readonly providerEventId: string;
  readonly outcome: 'conflict' | 'unmatched';
  // null for an unmatched update.
  readonly withdrawalId: Uuid<'wd'> | null;
  readonly reference: string;
  readonly status: string;
  readonly receivedAt: Date;
};

// The movements a withdrawal in each status must have, and no others. Alphabetical order, like
// expectedSteps, to match array_agg(... ORDER BY reference_step): ['execute', 'hold'] is
// deliberate, not lifecycle order.
function expectedWithdrawalSteps(status: WithdrawalStatus): readonly string[] {
  switch (status) {
    case 'held':
    case 'submitted':
      return ['hold'];
    case 'executed':
      return ['execute', 'hold'];
    case 'failed':
      return ['hold', 'release'];
  }
}

// The same per withdrawal (the same memory shape as PAYMENT_STEPS_SQL: a row per withdrawal).
const WITHDRAWAL_STEPS_SQL = `
SELECT w.id, w.account_id, w.status,
       COALESCE(array_agg(m.reference_step ORDER BY m.reference_step)
                  FILTER (WHERE m.id IS NOT NULL), '{}') AS steps,
       COALESCE(bool_or(m.account_id <> w.account_id), false) AS other_account,
       COALESCE(bool_or(m.amount_minor <> w.amount_minor), false) AS wrong_amount
FROM withdrawals w
LEFT JOIN balance_movements m ON m.reference_type = 'withdrawal' AND m.reference_id = w.id
WHERE $1::uuid[] IS NULL OR w.account_id = ANY($1)
GROUP BY w.id, w.account_id, w.status
`;

// Movements that reference a withdrawal that doesn't exist.
const ORPHAN_WITHDRAWAL_MOVEMENTS_SQL = `
SELECT m.reference_id, m.account_id, array_agg(m.reference_step ORDER BY m.reference_step) AS steps
FROM balance_movements m
LEFT JOIN withdrawals w ON w.id = m.reference_id
WHERE m.reference_type = 'withdrawal' AND w.id IS NULL
  AND ($1::uuid[] IS NULL OR m.account_id = ANY($1))
GROUP BY m.reference_id, m.account_id
`;

// Withdrawals held or submitted whose latest status change is older than the threshold.
const STUCK_WITHDRAWALS_SQL = `
SELECT w.id, w.account_id, w.status, h.at,
       floor(extract(epoch FROM now() - h.at))::int AS age_seconds
FROM withdrawals w
JOIN LATERAL (
  SELECT at FROM withdrawal_status_history WHERE withdrawal_id = w.id ORDER BY id DESC LIMIT 1
) h ON true
WHERE w.status = ANY($2::text[])
  AND h.at <= now() - $3 * interval '1 millisecond'
  AND ($1::uuid[] IS NULL OR w.account_id = ANY($1))
ORDER BY h.at
`;

const TRANSFER_UPDATE_PROBLEMS_SQL = `
SELECT u.provider_event_id, u.outcome, u.withdrawal_id, u.reference, u.status, u.received_at
FROM transfer_updates u
LEFT JOIN withdrawals w ON w.id = u.withdrawal_id
WHERE u.outcome IN ('conflict', 'unmatched')
  AND ($1::uuid[] IS NULL OR w.account_id = ANY($1))
ORDER BY u.received_at, u.id
`;

export async function checkWithdrawals(
  db: Queryable,
  scope: CheckScope,
): Promise<{
  readonly withdrawalMismatches: WithdrawalMismatch[];
  readonly withdrawalsChecked: number;
  readonly stuckWithdrawals: StuckWithdrawal[];
  readonly transferUpdateProblems: TransferUpdateProblem[];
}> {
  // Scale: as for payments, every withdrawal at once; only mismatches are kept.
  const withdrawalSteps = await db.query<{
    id: string;
    account_id: string;
    status: string;
    steps: string[];
    other_account: boolean;
    wrong_amount: boolean;
  }>(WITHDRAWAL_STEPS_SQL, [scope.accounts]);
  const withdrawalMismatches: WithdrawalMismatch[] = [];
  for (const row of withdrawalSteps.rows) {
    const status = parseWithdrawalStatus(row.status);
    const expected = expectedWithdrawalSteps(status);
    if (row.other_account || row.wrong_amount || !sameSteps(row.steps, expected)) {
      withdrawalMismatches.push({
        withdrawalId: row.id as Uuid<'wd'>,
        accountId: row.account_id as Uuid<'acct'>,
        status,
        steps: row.steps,
        expectedSteps: expected,
        otherAccount: row.other_account,
        wrongAmount: row.wrong_amount,
      });
    }
  }
  const withdrawalOrphans = await db.query<{
    reference_id: string;
    account_id: string;
    steps: string[];
  }>(ORPHAN_WITHDRAWAL_MOVEMENTS_SQL, [scope.accounts]);
  for (const row of withdrawalOrphans.rows) {
    withdrawalMismatches.push({
      withdrawalId: row.reference_id as Uuid<'wd'>,
      accountId: row.account_id as Uuid<'acct'>,
      status: null,
      steps: row.steps,
      expectedSteps: [],
      otherAccount: false,
      wrongAmount: false,
    });
  }

  const stuckWithdrawalRows = await db.query<{
    id: string;
    account_id: string;
    status: string;
    at: Date;
    age_seconds: number;
  }>(STUCK_WITHDRAWALS_SQL, [scope.accounts, UNSETTLED_WITHDRAWAL_STATUSES, scope.stuckAfterMs]);
  const stuckWithdrawals: StuckWithdrawal[] = stuckWithdrawalRows.rows.map((row) => ({
    withdrawalId: row.id as Uuid<'wd'>,
    accountId: row.account_id as Uuid<'acct'>,
    status: parseWithdrawalStatus(row.status),
    since: row.at,
    ageSeconds: row.age_seconds,
  }));

  const transferUpdates = await db.query<{
    provider_event_id: string;
    outcome: 'conflict' | 'unmatched';
    withdrawal_id: string | null;
    reference: string;
    status: string;
    received_at: Date;
  }>(TRANSFER_UPDATE_PROBLEMS_SQL, [scope.accounts]);
  const transferUpdateProblems: TransferUpdateProblem[] = transferUpdates.rows.map((row) => ({
    providerEventId: row.provider_event_id,
    outcome: row.outcome,
    withdrawalId: row.withdrawal_id as Uuid<'wd'> | null,
    reference: row.reference,
    status: row.status,
    receivedAt: row.received_at,
  }));

  return {
    withdrawalMismatches,
    withdrawalsChecked: withdrawalSteps.rows.length,
    stuckWithdrawals,
    transferUpdateProblems,
  };
}
