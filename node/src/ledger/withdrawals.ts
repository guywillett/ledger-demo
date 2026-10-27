import type { Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import {
  transitionWithdrawalStatus,
  UNSETTLED_WITHDRAWAL_STATUSES,
  type SettlementTransition,
  type SubmissionTransition,
} from '../withdrawals/status.ts';
import {
  insertWithdrawal,
  type NewWithdrawal,
  type Withdrawal,
} from '../withdrawals/withdrawals.ts';
import { moveOnce, OverdraftError, type Bucket, type WithdrawalStep } from './move.ts';

// Withdrawal postings: creating a withdrawal with its funds held, then executing or releasing
// them when it settles. Each one changes the withdrawal and posts its movement in the caller's
// transaction, so both happen or neither does. The deposits' postings are in posting.ts.

// Not enough available funds for the withdrawal: a business outcome (the client asked for more
// than the account holds), not a bug. Deliberately not an OverdraftError, which the deposit-worker
// treats as an invariant violation: only hold() turns one into the other, for the `available`
// bucket, where running short is expected.
export class InsufficientFundsError extends Error {
  readonly accountId: Uuid<'acct'>;
  readonly amountMinor: bigint;

  constructor(w: Pick<Withdrawal, 'id' | 'accountId' | 'amountMinor'>, cause: OverdraftError) {
    super(`insufficient available funds on account ${w.accountId} for withdrawal ${w.id}`, {
      cause,
    });
    this.name = 'InsufficientFundsError';
    this.accountId = w.accountId;
    this.amountMinor = w.amountMinor;
  }
}

// The only way to create a withdrawal: insert it in `held` and reserve its funds, available ->
// held, in the caller's transaction. One function, so a withdrawal can never be stored without
// its hold: `held` is one bucket per account, and settling a withdrawal that reserved nothing
// would execute or release other withdrawals' reserved funds.
//
// undefined when the account already has a withdrawal with this idempotency key (nothing is
// inserted or held; classifying the repeat is the caller's job). Throws InsufficientFundsError
// when available is short (see hold()).
export async function createWithdrawal(tx: Tx, w: NewWithdrawal): Promise<Withdrawal | undefined> {
  const withdrawal = await insertWithdrawal(tx, w);
  if (withdrawal === undefined) return undefined;
  await hold(tx, withdrawal);
  return withdrawal;
}

// available -> held for a withdrawal just inserted. The balance CHECK (available >= 0) is the
// insufficient-funds check, atomic with the reservation, so two concurrent withdrawals can never
// both spend the same funds (no check-then-act).
//
// Insufficient funds throws InsufficientFundsError rather than returning a result, because the
// transaction can't continue. When a statement fails, Postgres marks the whole transaction as
// aborted: every later statement in it fails ("current transaction is aborted, commands ignored
// until end of transaction block") until ROLLBACK. So the caller can't catch the error and carry
// on in the same transaction; it lets withTransaction roll back (the withdrawal row goes too, so
// the idempotency key stays free) and handles the error outside it (the API answers 422).
//
// Django is different on the surface: inside atomic(), a nested atomic() block is a SAVEPOINT,
// and an exception leaving it rolls back to the savepoint, so the outer transaction can carry on
// after catching it. Postgres has savepoints too, but nothing here uses them: a failed hold
// means the whole request failed, so rolling everything back is the behaviour we want.
async function hold(tx: Tx, w: Withdrawal): Promise<void> {
  try {
    // The withdrawal was inserted in this transaction, so its hold must be new (moveOnce).
    await moveOnce(tx, {
      accountId: w.accountId,
      from: 'available',
      to: 'held',
      amountMinor: w.amountMinor,
      referenceType: 'withdrawal',
      referenceId: w.id,
      referenceStep: 'hold',
    });
  } catch (err) {
    if (err instanceof OverdraftError && err.bucket === 'available') {
      throw new InsufficientFundsError(w, err);
    }
    throw err;
  }
}

// held -> submitted, recording the provider's transfer ID; no movement (the funds stay held
// until the transfer settles). 'skipped' if the withdrawal is no longer held: the provider's
// update may already have settled it (slice 2 decision 27), or this is a repeat.
export async function markSubmitted(
  tx: Tx,
  withdrawalId: Uuid<'wd'>,
  providerTransferId: string,
): Promise<'applied' | 'skipped'> {
  const t: SubmissionTransition = { from: ['held'], to: 'submitted', providerTransferId };
  const moved = await transitionWithdrawalStatus(tx, { withdrawalId, ...t });
  return moved === undefined ? 'skipped' : 'applied';
}

// The settlements optionally record the provider's transfer ID, which the provider's update
// carries: when it settles a withdrawal before the submission was recorded, markSubmitted then
// skips, and this is the only chance to store it. An ID that differs from the one already stored
// throws TransferIdMismatchError and changes nothing (withdrawals/status.ts); so does
// markSubmitted with another ID than the one an earlier update stored.

// held | submitted -> executed: the money has left, held -> external. 'skipped' if already
// settled (executed by a repeat, or failed).
export function execute(
  tx: Tx,
  withdrawalId: Uuid<'wd'>,
  providerTransferId?: string,
): Promise<'applied' | 'skipped'> {
  return settle(
    tx,
    withdrawalId,
    {
      from: UNSETTLED_WITHDRAWAL_STATUSES,
      to: 'executed',
      ...(providerTransferId === undefined ? {} : { providerTransferId }),
    },
    { from: 'held', to: 'external', step: 'execute' },
  );
}

// held | submitted -> failed, with the reason: the funds go back, held -> available. 'skipped'
// if already settled.
export function release(
  tx: Tx,
  withdrawalId: Uuid<'wd'>,
  reason: string,
  providerTransferId?: string,
): Promise<'applied' | 'skipped'> {
  return settle(
    tx,
    withdrawalId,
    {
      from: UNSETTLED_WITHDRAWAL_STATUSES,
      to: 'failed',
      reason,
      ...(providerTransferId === undefined ? {} : { providerTransferId }),
    },
    { from: 'held', to: 'available', step: 'release' },
  );
}

async function settle(
  tx: Tx,
  withdrawalId: Uuid<'wd'>,
  status: SettlementTransition,
  movement: { readonly from: Bucket; readonly to: Bucket; readonly step: WithdrawalStep },
): Promise<'applied' | 'skipped'> {
  const withdrawal = await transitionWithdrawalStatus(tx, { withdrawalId, ...status });
  if (withdrawal === undefined) return 'skipped';

  await moveOnce(tx, {
    accountId: withdrawal.accountId,
    from: movement.from,
    to: movement.to,
    amountMinor: withdrawal.amountMinor,
    referenceType: 'withdrawal',
    referenceId: withdrawalId,
    referenceStep: movement.step,
  });
  return 'applied';
}
