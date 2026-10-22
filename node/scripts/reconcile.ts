import { parseArgs } from 'node:util';
import { z } from 'zod';
import { reconcile, type Buckets, type ReconcileReport } from '../src/reconciliation/reconcile.ts';
import { parseEnv } from '../src/shared/config.ts';
import { createPool, withTransaction } from '../src/shared/db.ts';
import { fromTypeId, toTypeId, type Uuid } from '../src/shared/ids.ts';
import { createLogger } from '../src/shared/logger.ts';

// Reconciles the ledger: node scripts/reconcile.ts [options] (npm run reconcile).
//
//   --account acct_…     only this account (repeatable); default all accounts
//   --stuck-after <min>  report payments in a non-final status for longer (default 10)
//   --fail-on-stuck      exit 2 if any payment is stuck
//   --json               print the report as JSON instead of a table
//
// Exit codes: 0 consistent; 1 the books disagree (any difference or mismatch); 2 consistent
// but stuck payments and --fail-on-stuck; 3 couldn't run (bad arguments, database down).
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
    if (values['fail-on-stuck'] && report.stuck.length > 0) return EXIT.stuck;
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
      mismatches: report.mismatches.map((m) => ({
        ...m,
        paymentId: toTypeId('pay', m.paymentId),
        accountId: toTypeId('acct', m.accountId),
      })),
      stuck: report.stuck.map((s) => ({
        ...s,
        paymentId: toTypeId('pay', s.paymentId),
        accountId: toTypeId('acct', s.accountId),
      })),
    },
    (_key, value: unknown) => (typeof value === 'bigint' ? value.toString() : value),
    2,
  );
}

function toText(report: ReconcileReport, stuckAfterMinutes: number): string {
  const lines: string[] = [];
  lines.push(
    `Checked ${report.accountsChecked} accounts, ${report.paymentsChecked} payments: ` +
      (report.consistent ? 'the books agree.' : 'THE BOOKS DISAGREE.'),
  );

  if (report.differences.length > 0) {
    lines.push(
      '',
      'Accounts whose balance disagrees (minor units; balance / from payments / from movements):',
    );
    const show = (b: Buckets | null): string =>
      b === null ? '(no balance row)' : `${b.pendingIn}/${b.available}`;
    lines.push(
      table([
        ['account', 'pending_in/available', 'payments say', 'movements say'],
        ...report.differences.map((d) => [
          toTypeId('acct', d.accountId),
          show(d.balance),
          show(d.fromPayments),
          show(d.fromMovements),
        ]),
      ]),
    );
  }
  if (report.mismatches.length > 0) {
    lines.push('', 'Payments whose movements don’t match their status:');
    lines.push(
      table([
        ['payment', 'status', 'movements', 'expected', ''],
        ...report.mismatches.map((m) => [
          toTypeId('pay', m.paymentId),
          m.status ?? '(no such payment)',
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
    `Stuck (non-final for more than ${stuckAfterMinutes} min): ${report.stuck.length}`,
  );
  if (report.stuck.length > 0) {
    lines.push(
      table([
        ['payment', 'status', 'for'],
        ...report.stuck.map((s) => [
          toTypeId('pay', s.paymentId),
          s.status,
          `${Math.round(s.ageSeconds / 60)} min`,
        ]),
      ]),
    );
  }

  const outcomes = Object.entries(report.deliveries.byOutcome)
    .map(([outcome, n]) => `${outcome} ${n}`)
    .join(', ');
  lines.push(
    '',
    `Deliveries (all accounts): ${outcomes || 'none'}; in_progress for more than ${stuckAfterMinutes} min: ${report.deliveries.staleInProgress}`,
    `Duplicate notifications: ${report.deliveries.duplicateNotifications}`,
  );
  return lines.join('\n');
}

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
