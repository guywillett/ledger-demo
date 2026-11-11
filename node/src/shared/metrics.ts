import { metrics, type Counter } from '@opentelemetry/api';
import { afterCommit, type Tx } from './db.ts';

// ledger_transitions_total (slice 5 decision 88, contracts/telemetry.md): one per applied status
// change of a deposit or a withdrawal, its creation included (from "none": the pipeline's entry),
// counted once its transaction has committed (afterCommit): a skipped transition (already done)
// isn't applied, so it isn't counted, and a rolled-back transaction counts nothing. Its attributes
// are bounded: the legal transitions, and the implementation. Go: internal/telemetry's counter.

export type TransitionEntity = 'deposit' | 'withdrawal';

let counter: Counter | undefined;

// Created on first use, so from the SDK's meter provider: every process starts telemetry before it
// loads this (bin/). The JavaScript API, unlike Go's, doesn't hand an instrument made before a
// provider is registered over to it: it would stay a no-op.
function transitions(): Counter {
  counter ??= metrics.getMeter('ledger-demo/ledger').createCounter('ledger_transitions_total', {
    unit: '{transition}',
    description:
      'Applied status transitions of deposits and withdrawals, their creation included, ' +
      'counted once each after their transaction commits.',
  });
  return counter;
}

// Counts the transition once tx commits. from is null for a creation (counted as "none").
// implementation is an attribute of its own, not only the resource's: otel-lgtm's Prometheus puts
// resource attributes on target_info, not on each series, and without an instance to join on, a
// Node and a Go process of one role would otherwise write the same series.
export function countTransition(
  tx: Tx,
  t: { readonly entity: TransitionEntity; readonly from: string | null; readonly to: string },
): void {
  afterCommit(tx, () => {
    transitions().add(1, {
      entity: t.entity,
      from: t.from ?? 'none',
      to: t.to,
      implementation: 'node',
    });
  });
}
