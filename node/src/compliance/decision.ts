import type { Decision } from './transitions.ts';

// The pseudo compliance check: approve with probability approveRate, otherwise reject. Not a
// rules engine (see Known gaps). `random` is injectable so tests can force either outcome.
export function decideRandomly(approveRate: number, random: () => number = Math.random): Decision {
  return random() < approveRate
    ? { outcome: 'approved' }
    : { outcome: 'rejected', reason: 'rejected by the demo compliance check (random)' };
}
