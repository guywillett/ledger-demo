import { setTimeout as sleep } from 'node:timers/promises';

// Retry timing shared by the long-running processes (worker, relay).

export type Backoff = {
  readonly retryBaseDelayMs: number;
  readonly retryMaxDelayMs: number;
};

// retryBaseDelayMs on the first attempt, doubling with each further attempt, at most
// retryMaxDelayMs.
export function retryDelay(backoff: Backoff, attempt: number): number {
  const exponent = Math.min(Math.max(attempt - 1, 0), 30);
  return Math.min(backoff.retryBaseDelayMs * 2 ** exponent, backoff.retryMaxDelayMs);
}

// Waits, but returns early when `signal` aborts (on shutdown).
export async function pause(ms: number, signal: AbortSignal): Promise<void> {
  try {
    await sleep(ms, undefined, { signal });
  } catch {
    // Aborted: shutting down.
  }
}
