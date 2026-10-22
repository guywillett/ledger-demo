import { describe, expect, it } from 'vitest';
import { retryDelay } from '../../src/shared/backoff.ts';

describe('retryDelay', () => {
  it('doubles with each attempt, up to the maximum', () => {
    const limits = { retryBaseDelayMs: 1000, retryMaxDelayMs: 60_000 };
    expect([1, 2, 3, 4, 6, 7, 50].map((attempt) => retryDelay(limits, attempt))).toEqual([
      1000, 2000, 4000, 8000, 32_000, 60_000, 60_000,
    ]);
  });
});
