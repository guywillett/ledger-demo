import type { Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import {
  type PostingTransition,
  type TransitionResult,
  transitionStatus,
} from '../deposits/status.ts';
import { moveOnce, type Bucket, type DepositStep } from './move.ts';

// Posting: the transitions that move money. Each one changes the deposit's status and posts
// its movement in the caller's transaction, so both happen or neither does. The compliance
// transitions in between (review, decision) move no money and live in compliance/.

// Withdrawal postings are in withdrawals.ts. A transition whose movement already exists throws
// LedgerInconsistencyError (moveOnce, move.ts).

// A posting step: one of posting's own status transitions, and the movement it posts.
type PostingStep = {
  readonly status: PostingTransition;
  readonly movement: { readonly from: Bucket; readonly to: Bucket; readonly step: DepositStep };
};

const BOOK: PostingStep = {
  status: { from: 'created', to: 'received' },
  movement: { from: 'external', to: 'pending_in', step: 'book' },
};
const CREDIT: PostingStep = {
  status: { from: 'approved', to: 'credited' },
  movement: { from: 'pending_in', to: 'available', step: 'credit' },
};
const RETURN: PostingStep = {
  status: { from: 'rejected', to: 'returned' },
  movement: { from: 'pending_in', to: 'external', step: 'return' },
};

// created -> received: the funds are booked into pending_in.
export function book(tx: Tx, depositId: Uuid<'dep'>): Promise<TransitionResult> {
  return post(tx, depositId, BOOK);
}

// approved -> credited: pending_in -> available.
export function credit(tx: Tx, depositId: Uuid<'dep'>): Promise<TransitionResult> {
  return post(tx, depositId, CREDIT);
}

// rejected -> returned: pending_in -> external (returned to sender).
export function returnToSender(tx: Tx, depositId: Uuid<'dep'>): Promise<TransitionResult> {
  return post(tx, depositId, RETURN);
}

// Posts whatever compliance decided, reading the decision from the deposit's recorded state:
// tries both conditional transitions, and at most one can match. 'skipped' if neither does
// (not decided yet, or already posted).
export async function postDecision(
  tx: Tx,
  depositId: Uuid<'dep'>,
): Promise<'credited' | 'returned' | 'skipped'> {
  if ((await credit(tx, depositId)) === 'applied') return 'credited';
  if ((await returnToSender(tx, depositId)) === 'applied') return 'returned';
  return 'skipped';
}

async function post(tx: Tx, depositId: Uuid<'dep'>, t: PostingStep): Promise<TransitionResult> {
  const deposit = await transitionStatus(tx, { depositId, ...t.status });
  if (deposit === undefined) return 'skipped';

  await moveOnce(tx, {
    accountId: deposit.accountId,
    from: t.movement.from,
    to: t.movement.to,
    amountMinor: deposit.amountMinor,
    referenceType: 'deposit',
    referenceId: depositId,
    referenceStep: t.movement.step,
  });
  return 'applied';
}
