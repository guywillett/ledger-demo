import { parseArgs } from 'node:util';
import { z } from 'zod';
import {
  reconcile,
  type Buckets,
  type CreatedEventState,
  type ReconcileReport,
} from '../src/reconciliation/reconcile.ts';
import { parseEnv } from '../src/shared/config.ts';
import { createPool, withTransaction } from '../src/shared/db.ts';
import { fromTypeId, toTypeId, type Uuid } from '../src/shared/ids.ts';
import { createLogger } from '../src/shared/logger.ts';

// Reconciles the ledger: node scripts/reconcile.ts [options] (npm run reconcile).
//
//   --account acct_…     only this account (repeatable); default all accounts
//   --stuck-after <min>  report deposits and withdrawals in a non-final status, and events
//                        unpublished, for longer (default 10)
//   --fail-on-stuck      exit 2 if any deposit, withdrawal or event is stuck
//   --json               print the report as JSON instead of a table
//
// Exit codes: 0 consistent; 1 the books disagree (any difference or mismatch); 2 consistent
// but stuck deposits, withdrawals or events and --fail-on-stuck; 3 couldn't run (bad arguments, database down).
// Reads DATABASE_URL (and LOG_LEVEL) like the services; no NATS needed.

const EXIT = { consistent: 0, inconsistent: 1, stuck: 2, error: 3 } as const;

const envSchema = z.object({ DATABASE_URL: z.url() });

async function main(): Promise<number> {
  const { values } = parseArgs({
    options: {
      account: { type: 'string', multiple: true },
      'stuck-after': { type: 'string', default: '10' },
      'fail-on-stuck': { type: 'boolean', default: false },
      json: { type: 'boolean', default: false },
    },
    strict: true,
  });
  const stuckAfterMinutes = Number(values['stuck-after']);
  if (!Number.isFinite(stuckAfterMinutes) || stuckAfterMinutes < 0) {
    throw new UsageError('--stuck-after must be a number of minutes, 0 or more');
  }
  const accountIds = values.account?.map((id) => parseAccount(id));

  const env = parseEnv(envSchema, process.env);
  const pool = createPool({
    databaseUrl: env.DATABASE_URL,
    logger: createLogger({ service: 'reconcile', level: 'silent' }),
  });
  try {
    // One snapshot for every query, read-only.
    const report = await withTransaction(
      pool,
      (tx) =>
        reconcile(tx, {
          stuckAfterMs: stuckAfterMinutes * 60_000,
          ...(accountIds === undefined ? {} : { accountIds }),
        }),
      { isolation: 'repeatable read', readOnly: true },
    );
    console.log(values.json ? toJson(report) : toText(report, stuckAfterMinutes));
    if (!report.consistent) return EXIT.inconsistent;
    const stuck =
      report.stuckDeposits.length > 0 ||
      report.stuckWithdrawals.length > 0 ||
      report.outbox.stuckUnpublished > 0;
    if (values['fail-on-stuck'] && stuck) return EXIT.stuck;
    return EXIT.consistent;
  } finally {
    await pool.end();
  }
}

class UsageError extends Error {}

function parseAccount(id: string): Uuid<'acct'> {
  try {
    return fromTypeId('acct', id);
  } catch {
    throw new UsageError(`--account: not an acct_ ID: ${id}`);
  }
}

// BigInts as digit strings and IDs as TypeIDs, like the API.
function toJson(report: ReconcileReport): string {
  return JSON.stringify(
    {
      ...report,
      differences: report.differences.map((d) => ({
        ...d,
        accountId: toTypeId('acct', d.accountId),
      })),
      depositMismatches: report.depositMismatches.map((m) => ({
        ...m,
        depositId: toTypeId('dep', m.depositId),
        accountId: toTypeId('acct', m.accountId),
      })),
      stuckDeposits: report.stuckDeposits.map((s) => ({
        ...s,
        depositId: toTypeId('dep', s.depositId),
        accountId: toTypeId('acct', s.accountId),
      })),
      withdrawalMismatches: report.withdrawalMismatches.map((m) => ({
        ...m,
        withdrawalId: toTypeId('wd', m.withdrawalId),
        accountId: toTypeId('acct', m.accountId),
      })),
      stuckWithdrawals: report.stuckWithdrawals.map((s) => ({
        ...s,
        withdrawalId: toTypeId('wd', s.withdrawalId),
        accountId: toTypeId('acct', s.accountId),
      })),
      suspense:
        report.suspense === null
          ? null
          : {
              ...report.suspense,
              deposits: report.suspense.deposits.map((p) => ({
                ...p,
                depositId: toTypeId('dep', p.depositId),
              })),
            },
      depositConflicts: report.depositConflicts.map((c) => ({
        ...c,
        depositId: toTypeId('dep', c.depositId),
      })),
      transferUpdateProblems: report.transferUpdateProblems.map((p) => ({
        ...p,
        withdrawalId: p.withdrawalId === null ? null : toTypeId('wd', p.withdrawalId),
      })),
    },
    (_key, value: unknown) => (typeof value === 'bigint' ? value.toString() : value),
    2,
  );
}

function toText(report: ReconcileReport, stuckAfterMinutes: number): string {
  const lines: string[] = [];
  lines.push(
    `Checked ${report.accountsChecked} accounts, ${report.depositsChecked} deposits, ` +
      `${report.withdrawalsChecked} withdrawals: ` +
      (report.consistent ? 'the books agree.' : 'THE BOOKS DISAGREE.'),
  );

  if (report.differences.length > 0) {
    lines.push(
      '',
      'Accounts whose balance disagrees (minor units; balance / from statuses / from movements):',
    );
    const show = (b: Buckets | null): string =>
      b === null ? '(no balance row)' : `${b.pendingIn}/${b.available}/${b.held}`;
    lines.push(
      table([
        ['account', 'pending_in/available/held', 'statuses say', 'movements say'],
        ...report.differences.map((d) => [
          toTypeId('acct', d.accountId),
          show(d.balance),
          show(d.fromStatuses),
          show(d.fromMovements),
        ]),
      ]),
    );
  }
  if (report.depositMismatches.length > 0) {
    lines.push('', 'Deposits whose movements don’t match their status:');
    lines.push(
      table([
        ['deposit', 'status', 'movements', 'expected', ''],
        ...report.depositMismatches.map((m) => [
          toTypeId('dep', m.depositId),
          m.status ?? '(no such deposit)',
          m.steps.join(',') || '-',
          m.expectedSteps.join(',') || '-',
          [m.otherAccount ? 'posted to another account' : '', m.wrongAmount ? 'wrong amount' : '']
            .filter((note) => note !== '')
            .join('; '),
        ]),
      ]),
    );
  }

  if (report.withdrawalMismatches.length > 0) {
    lines.push('', 'Withdrawals whose movements don’t match their status:');
    lines.push(
      table([
        ['withdrawal', 'status', 'movements', 'expected', ''],
        ...report.withdrawalMismatches.map((m) => [
          toTypeId('wd', m.withdrawalId),
          m.status ?? '(no such withdrawal)',
          m.steps.join(',') || '-',
          m.expectedSteps.join(',') || '-',
          [m.otherAccount ? 'posted to another account' : '', m.wrongAmount ? 'wrong amount' : '']
            .filter((note) => note !== '')
            .join('; '),
        ]),
      ]),
    );
  }

  lines.push(
    '',
    `Stuck (non-final for more than ${stuckAfterMinutes} min): ${report.stuckDeposits.length}`,
  );
  if (report.stuckDeposits.length > 0) {
    lines.push(
      table([
        ['deposit', 'status', 'for', 'deposit.created'],
        ...report.stuckDeposits.map((s) => [
          toTypeId('dep', s.depositId),
          s.status,
          `${Math.round(s.ageSeconds / 60)} min`,
          s.event === null ? '' : EVENT_STATE[s.event],
        ]),
      ]),
    );
  }

  lines.push(
    '',
    `Stuck withdrawals (held or submitted for more than ${stuckAfterMinutes} min): ` +
      `${report.stuckWithdrawals.length}`,
  );
  if (report.stuckWithdrawals.length > 0) {
    lines.push(
      table([
        ['withdrawal', 'status', 'for'],
        ...report.stuckWithdrawals.map((s) => [
          toTypeId('wd', s.withdrawalId),
          s.status,
          `${Math.round(s.ageSeconds / 60)} min`,
        ]),
      ]),
    );
  }

  const { suspense } = report;
  if (suspense === null) {
    lines.push(
      '',
      'Suspense: not checked (account-scoped run; unmatched deposits have no account)',
    );
  } else {
    lines.push(
      '',
      `Suspense (unmatched deposits, all accounts): ${suspense.count}` +
        (suspense.count === 0
          ? ''
          : `, total ${suspense.totalMinor} minor units, oldest ${suspense.oldestAgeSeconds} s`),
    );
    if (suspense.count > 0) {
      lines.push(
        table([
          ['deposit', 'network transaction', 'amount', 'age'],
          ...suspense.deposits.map((p) => [
            toTypeId('dep', p.depositId),
            p.networkTransactionId,
            p.amountMinor.toString(),
            `${p.ageSeconds} s`,
          ]),
        ]),
      );
    }
  }

  lines.push('', `Deposit notification conflicts: ${report.depositConflicts.length}`);
  if (report.depositConflicts.length > 0) {
    lines.push(
      table([
        ['deposit', 'differing fields', 'received'],
        ...report.depositConflicts.map((c) => [
          toTypeId('dep', c.depositId),
          c.differingFields.join(','),
          c.receivedAt.toISOString(),
        ]),
      ]),
    );
  }

  lines.push(
    '',
    `Transfer updates for a person to look at (conflicts, unmatched): ` +
      `${report.transferUpdateProblems.length}`,
  );
  if (report.transferUpdateProblems.length > 0) {
    lines.push(
      table([
        ['provider event', 'outcome', 'withdrawal', 'status', 'received'],
        ...report.transferUpdateProblems.map((p) => [
          p.providerEventId,
          p.outcome,
          p.withdrawalId === null ? `(none: ${p.reference})` : toTypeId('wd', p.withdrawalId),
          p.status,
          p.receivedAt.toISOString(),
        ]),
      ]),
    );
  }

  const { outbox } = report;
  lines.push(
    '',
    `Outbox: unpublished for more than ${stuckAfterMinutes} min: ${outbox.stuckUnpublished}` +
      (outbox.oldestUnpublishedAgeSeconds === null
        ? '; everything published'
        : `; oldest unpublished: ${outbox.oldestUnpublishedAgeSeconds} s; ` +
          `most attempts: ${outbox.maxUnpublishedAttempts}`),
  );

  // One line per consumer: outcome counts, then the in_progress rows older than the threshold.
  const consumers = Object.entries(report.deliveries.byConsumer);
  lines.push('', `Deliveries (all accounts):${consumers.length === 0 ? ' none' : ''}`);
  for (const [consumer, outcomes] of consumers) {
    const counts = Object.entries(outcomes)
      .map(([outcome, n]) => `${outcome} ${n}`)
      .join(', ');
    const stale = report.deliveries.staleInProgress[consumer] ?? 0;
    lines.push(
      `  ${consumer}: ${counts}; in_progress for more than ${stuckAfterMinutes} min: ${stale}`,
    );
  }
  lines.push(`Duplicate notifications: ${report.deliveries.duplicateNotifications}`);
  return lines.join('\n');
}

// Which side stalled, for a deposit stuck in `created`.
const EVENT_STATE: Record<CreatedEventState, string> = {
  unpublished: 'unpublished (relay)',
  published: 'published (deposit-worker)',
  missing: 'MISSING (bug: stored without its event)',
};

function table(rows: string[][]): string {
  const widths =
    rows[0]?.map((_, col) => Math.max(...rows.map((r) => (r[col] ?? '').length))) ?? [];
  return rows
    .map(
      (r) =>
        '  ' +
        r
          .map((cell, col) => cell.padEnd(widths[col] ?? 0))
          .join('  ')
          .trimEnd(),
    )
    .join('\n');
}

try {
  process.exitCode = await main();
} catch (err) {
  console.error(err instanceof UsageError ? err.message : err);
  process.exitCode = EXIT.error;
}
