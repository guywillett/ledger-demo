import type { Tx } from '../shared/db.ts';
import { newUuid, type Uuid } from '../shared/ids.ts';

// The only code that changes balances. Every change is a movement between two buckets of one
// account, recorded in balance_movements (append-only) and applied to account_balances in the
// caller's transaction. `external` is outside our books: money arriving or leaving.

export type Bucket = 'external' | 'pending_in' | 'available' | 'held';
export type PaymentStep = 'book' | 'credit' | 'return';
export type WithdrawalStep = 'hold' | 'execute' | 'release';

// The business object causing a movement, and the step within it. A discriminated union that
// mirrors the database's paired CHECK on (reference_type, reference_step): referenceType decides
// which ID and which steps are allowed, so a payment reference with a withdrawal step (or a
// withdrawal's UUID) is a compile error.
export type MovementReference =
  | {
      readonly referenceType: 'payment';
      readonly referenceId: Uuid<'pay'>;
      readonly referenceStep: PaymentStep;
    }
  | {
      readonly referenceType: 'withdrawal';
      readonly referenceId: Uuid<'wd'>;
      readonly referenceStep: WithdrawalStep;
    };

// A given reference (type, id, step) is applied at most once, whatever the caller does.
export type Movement = {
  readonly accountId: Uuid<'acct'>;
  readonly from: Bucket;
  readonly to: Bucket;
  readonly amountMinor: bigint;
} & MovementReference;

// A posting's movement already exists although the entity's state says the step is happening
// for the first time (its transition just applied, or it was just inserted). Two guards
// disagree. Thrown so the transaction rolls back (the status change is undone) and the caller
// logs it; never silently accepted.
export class LedgerInconsistencyError extends Error {
  constructor(r: MovementReference) {
    super(
      `${r.referenceType} ${r.referenceId}: its ${r.referenceStep} movement already exists, ` +
        `but the ${r.referenceType}'s state says that step hasn't happened yet`,
    );
    this.name = 'LedgerInconsistencyError';
  }
}

// A movement with this reference already exists but differs (another account, amount or
// buckets): the reference is being reused for something else, which is a bug, not a retry.
export class MovementConflictError extends Error {
  constructor(m: Movement) {
    super(
      `movement ${m.referenceType}/${m.referenceId}/${m.referenceStep} already exists ` +
        'with different details',
    );
    this.name = 'MovementConflictError';
  }
}

// The movement would take a bucket below zero: the CHECK (... >= 0) on account_balances
// rejected it. Posting can only overdraw through a bug, which is exactly when the log should
// say what happened rather than show a raw database error.
export class OverdraftError extends Error {
  readonly accountId: Uuid<'acct'>;
  readonly bucket: Bucket;
  readonly amountMinor: bigint;

  constructor(m: Movement, bucket: Bucket, cause: unknown) {
    super(
      `movement ${m.referenceType}/${m.referenceId}/${m.referenceStep} would overdraw ` +
        `${bucket} of account ${m.accountId}`,
      { cause },
    );
    this.name = 'OverdraftError';
    this.accountId = m.accountId;
    this.bucket = bucket;
    this.amountMinor = m.amountMinor;
  }
}

// Postgres names a column's CHECK constraint <table>_<column>_check.
const BALANCE_CHECKS: Readonly<Record<string, Bucket>> = {
  account_balances_pending_in_check: 'pending_in',
  account_balances_available_check: 'available',
  account_balances_held_check: 'held',
};

// A check_violation (SQLSTATE 23514) on one of the balance CHECKs becomes an OverdraftError;
// anything else is returned unchanged, to be rethrown as it was.
function asOverdraft(err: unknown, m: Movement): unknown {
  if (typeof err !== 'object' || err === null) return err;
  if (!('code' in err) || err.code !== '23514') return err;
  if (!('constraint' in err) || typeof err.constraint !== 'string') return err;
  const bucket = BALANCE_CHECKS[err.constraint];
  return bucket === undefined ? err : new OverdraftError(m, bucket, err);
}

const INSERT_MOVEMENT_SQL = `
INSERT INTO balance_movements (id, account_id, from_bucket, to_bucket, amount_minor,
                               reference_type, reference_id, reference_step)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (reference_type, reference_id, reference_step) DO NOTHING
RETURNING id
`;

const EXISTING_MOVEMENT_SQL = `
SELECT account_id, from_bucket, to_bucket, amount_minor
FROM balance_movements
WHERE reference_type = $1 AND reference_id = $2 AND reference_step = $3
`;

// One fixed statement for every movement: the change to each bucket is computed in TypeScript
// (minus for the source, plus for the destination, nothing for external), so the SQL never
// changes shape and no column names are built from strings. CHECK (... >= 0) on each column
// rejects a movement that would overdraw a bucket (reported as an OverdraftError).
const APPLY_TO_BALANCES_SQL = `
UPDATE account_balances
SET pending_in = pending_in + $2,
    available  = available  + $3,
    held       = held       + $4,
    updated_at = now()
WHERE account_id = $1
`;

// Applies the movement once: 'applied' if this call recorded it, 'duplicate' if a movement
// with the same reference already exists (and is the same movement). Must run inside the
// caller's transaction, so the movement, the balance change and whatever caused them commit
// or roll back together.
//
// Concurrency: if another transaction is inserting the same reference, ON CONFLICT waits for
// it; if it commits, this call is a duplicate, if it rolls back, this call applies.
export async function move(tx: Tx, m: Movement): Promise<'applied' | 'duplicate'> {
  if (m.from === m.to) throw new Error(`movement from ${m.from} to itself`);
  if (m.amountMinor <= 0n) throw new Error('movement amount must be positive');

  const inserted = await tx.query(INSERT_MOVEMENT_SQL, [
    newUuid('mov'),
    m.accountId,
    m.from,
    m.to,
    m.amountMinor,
    m.referenceType,
    m.referenceId,
    m.referenceStep,
  ]);
  if (inserted.rowCount === 0) {
    await expectSameMovement(tx, m);
    return 'duplicate';
  }

  const balances = await tx
    .query(APPLY_TO_BALANCES_SQL, [
      m.accountId,
      change('pending_in', m),
      change('available', m),
      change('held', m),
    ])
    .catch((err: unknown) => {
      throw asOverdraft(err, m);
    });
  if (balances.rowCount !== 1) {
    throw new Error(`no balance row for account ${m.accountId}`);
  }
  return 'applied';
}

// move() for a posting: the caller has just established, through the entity's own state (a
// conditional transition that applied, or a fresh insert), that this step is happening for the
// first time. So the movement must be new, and a duplicate means the two guards disagree.
export async function moveOnce(tx: Tx, m: Movement): Promise<void> {
  if ((await move(tx, m)) === 'duplicate') throw new LedgerInconsistencyError(m);
}

function change(bucket: Bucket, m: Movement): bigint {
  let delta = 0n;
  if (m.from === bucket) delta -= m.amountMinor;
  if (m.to === bucket) delta += m.amountMinor;
  return delta;
}

// The duplicate path reads the existing movement (it sees the latest committed data, or this
// transaction's own earlier insert) and refuses a reference reused with other details.
async function expectSameMovement(tx: Tx, m: Movement): Promise<void> {
  const result = await tx.query<{
    account_id: string;
    from_bucket: string;
    to_bucket: string;
    amount_minor: bigint;
  }>(EXISTING_MOVEMENT_SQL, [m.referenceType, m.referenceId, m.referenceStep]);
  const existing = result.rows[0];
  if (
    existing === undefined ||
    existing.account_id !== m.accountId ||
    existing.from_bucket !== m.from ||
    existing.to_bucket !== m.to ||
    existing.amount_minor !== m.amountMinor
  ) {
    throw new MovementConflictError(m);
  }
}
