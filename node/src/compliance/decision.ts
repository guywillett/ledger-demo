// The compliance decision: what was decided, and how (for now, at random).

// A rejection always carries a reason; an approval may.
export type Decision =
  | { readonly outcome: 'approved'; readonly reason?: string }
  | { readonly outcome: 'rejected'; readonly reason: string };

// The pseudo compliance check: approve with probability approveRate, otherwise reject. Not a
// rules engine (see Known gaps). `random` is injectable so tests can force either outcome.
export function decideRandomly(approveRate: number, random: () => number = Math.random): Decision {
  return random() < approveRate
    ? { outcome: 'approved' }
    : { outcome: 'rejected', reason: 'rejected by the demo compliance check (random)' };
}
