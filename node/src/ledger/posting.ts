import type { Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import {
  transitionStatus,
  type PostingTransition,
  type TransitionResult,
} from '../shared/payment-status.ts';
import { move, type Bucket, type ReferenceStep } from './move.ts';

// Posting: the transitions that move money. Each one changes the payment's status and posts
// its movement in the caller's transaction, so both happen or neither does. The compliance
// transitions in between (review, decision) move no money and live in compliance/.

// The transition happened but its movement already exists. Two guards disagree: payment state
// says this is the first time, the ledger says it isn't. Thrown so the transaction rolls back
// (the status change is undone) and the caller logs it; never silently accepted.
export class LedgerInconsistencyError extends Error {
  constructor(paymentId: Uuid<'pay'>, step: ReferenceStep) {
    super(`payment ${paymentId} moved status, but its ${step} movement already exists`);
    this.name = 'LedgerInconsistencyError';
  }
}

// A posting step: one of posting's own status transitions, and the movement it posts.
type PostingStep = {
  readonly status: PostingTransition;
  readonly movement: { readonly from: Bucket; readonly to: Bucket; readonly step: ReferenceStep };
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
export function book(tx: Tx, paymentId: Uuid<'pay'>): Promise<TransitionResult> {
  return post(tx, paymentId, BOOK);
}

// approved -> credited: pending_in -> available.
export function credit(tx: Tx, paymentId: Uuid<'pay'>): Promise<TransitionResult> {
  return post(tx, paymentId, CREDIT);
}

// rejected -> returned: pending_in -> external (returned to sender).
export function returnToSender(tx: Tx, paymentId: Uuid<'pay'>): Promise<TransitionResult> {
  return post(tx, paymentId, RETURN);
}

// Posts whatever compliance decided, reading the decision from the payment's recorded state:
// tries both conditional transitions, and at most one can match. 'skipped' if neither does
// (not decided yet, or already posted).
export async function postDecision(
  tx: Tx,
  paymentId: Uuid<'pay'>,
): Promise<'credited' | 'returned' | 'skipped'> {
  if ((await credit(tx, paymentId)) === 'applied') return 'credited';
  if ((await returnToSender(tx, paymentId)) === 'applied') return 'returned';
  return 'skipped';
}

async function post(tx: Tx, paymentId: Uuid<'pay'>, t: PostingStep): Promise<TransitionResult> {
  const payment = await transitionStatus(tx, { paymentId, ...t.status });
  if (payment === undefined) return 'skipped';

  const outcome = await move(tx, {
    accountId: payment.accountId,
    from: t.movement.from,
    to: t.movement.to,
    amountMinor: payment.amountMinor,
    referenceType: 'payment',
    referenceId: paymentId,
    referenceStep: t.movement.step,
  });
  if (outcome === 'duplicate') throw new LedgerInconsistencyError(paymentId, t.movement.step);
  return 'applied';
}
