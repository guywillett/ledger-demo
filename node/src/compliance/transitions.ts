import type { Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import {
  transitionStatus,
  type ComplianceTransition,
  type TransitionResult,
} from '../shared/payment-status.ts';

// Compliance: the transitions it owns. They record the review and its decision and never move
// money; posting the decision (crediting or returning the funds) is the ledger's job
// (ledger/posting.ts). How a decision is reached lives elsewhere in this module.

// A rejection always carries a reason; an approval may.
export type Decision =
  | { readonly outcome: 'approved'; readonly reason?: string }
  | { readonly outcome: 'rejected'; readonly reason: string };

// received -> under_review: compliance has picked the payment up.
export function startReview(tx: Tx, paymentId: Uuid<'pay'>): Promise<TransitionResult> {
  const review: ComplianceTransition = { from: 'received', to: 'under_review' };
  return applied(transitionStatus(tx, { paymentId, ...review }));
}

// under_review -> approved | rejected. 'skipped' if the payment isn't under review, in
// particular if it has already been decided: a recorded decision is never re-made.
export function decide(
  tx: Tx,
  paymentId: Uuid<'pay'>,
  decision: Decision,
): Promise<TransitionResult> {
  const decided: ComplianceTransition = { from: 'under_review', to: decision.outcome };
  return applied(
    transitionStatus(tx, {
      paymentId,
      ...decided,
      ...(decision.reason === undefined ? {} : { reason: decision.reason }),
    }),
  );
}

async function applied(transition: Promise<unknown>): Promise<TransitionResult> {
  return (await transition) === undefined ? 'skipped' : 'applied';
}
