import {
  AggregationTemporality,
  InMemoryMetricExporter,
  PeriodicExportingMetricReader,
} from '@opentelemetry/sdk-metrics';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import { afterAll, describe, expect, it } from 'vitest';
import { startTelemetry } from '../../src/shared/telemetry.ts';

// ledger_transitions_total (slice 5 decision 88), through startTelemetry's real views: an applied
// transition counts once with its attributes, a skipped one (a duplicate delivery) nothing, a
// rolled-back transaction nothing, and a creation counts from "none". Counts are compared before
// and after each step: the counter is the process's, cumulative.
const metrics = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
const metricReader = new PeriodicExportingMetricReader({
  exporter: metrics,
  exportIntervalMillis: 3_600_000, // only when a test flushes
});
startTelemetry({
  service: 'deposit-worker',
  spanExporter: new InMemorySpanExporter(),
  metricReader,
});

const helpers = await import('../helpers.ts');
const { book } = await import('../../src/ledger/posting.ts');
const { markSubmitted } = await import('../../src/ledger/withdrawals.ts');
const { withTransaction } = await import('../../src/shared/db.ts');

const pool = helpers.createTestPool();
afterAll(() => pool.end());

type Step = { entity: string; from: string; to: string };

// The counter's value for one transition, as exported now; 0 before its first count.
async function count(step: Step): Promise<number> {
  metrics.reset();
  await metricReader.forceFlush();
  for (const metric of metrics
    .getMetrics()
    .flatMap((rm) => rm.scopeMetrics.flatMap((sm) => sm.metrics))) {
    if (metric.descriptor.name !== 'ledger_transitions_total') continue;
    for (const point of metric.dataPoints) {
      const { entity, from, to } = point.attributes;
      if (entity === step.entity && from === step.from && to === step.to) {
        return typeof point.value === 'number' ? point.value : 0;
      }
    }
  }
  return 0;
}

// How much the counter for step grows while fn runs.
async function counted(step: Step, fn: () => Promise<unknown>): Promise<number> {
  const before = await count(step);
  await fn();
  return (await count(step)) - before;
}

const CREATED = { entity: 'deposit', from: 'none', to: 'created' };
const BOOKED = { entity: 'deposit', from: 'created', to: 'received' };

describe('ledger_transitions_total', () => {
  it('counts a creation from "none", for deposits and withdrawals', async () => {
    const account = await helpers.createTestAccount(pool);
    expect(await counted(CREATED, () => helpers.createTestDeposit(pool, account))).toBe(1);
    await helpers.fundTestAccount(pool, account, 1000n);
    expect(
      await counted({ entity: 'withdrawal', from: 'none', to: 'held' }, () =>
        helpers.createTestWithdrawal(pool, account, 400n),
      ),
    ).toBe(1);
  });

  it('counts an applied transition once, with its attributes', async () => {
    const deposit = await helpers.createTestDeposit(pool, await helpers.createTestAccount(pool));
    expect(await counted(BOOKED, () => withTransaction(pool, (tx) => book(tx, deposit.id)))).toBe(
      1,
    );

    metrics.reset();
    await metricReader.forceFlush();
    const point = metrics
      .getMetrics()
      .flatMap((rm) => rm.scopeMetrics.flatMap((sm) => sm.metrics))
      .find((m) => m.descriptor.name === 'ledger_transitions_total');
    expect(point?.descriptor).toMatchObject({ unit: '{transition}' });
    expect(point?.dataPoints.map((p) => p.attributes)).toContainEqual({
      ...BOOKED,
      implementation: 'node',
    });
  });

  it('counts nothing for a skipped transition (a duplicate delivery)', async () => {
    const deposit = await helpers.createTestDeposit(pool, await helpers.createTestAccount(pool));
    await withTransaction(pool, (tx) => book(tx, deposit.id));
    expect(
      await counted(BOOKED, async () => {
        expect(await withTransaction(pool, (tx) => book(tx, deposit.id))).toBe('skipped');
      }),
    ).toBe(0);
  });

  it('counts nothing for a transaction that rolls back after the transition', async () => {
    const deposit = await helpers.createTestDeposit(pool, await helpers.createTestAccount(pool));
    expect(
      await counted(BOOKED, async () => {
        await expect(
          withTransaction(pool, async (tx) => {
            expect(await book(tx, deposit.id)).toBe('applied');
            throw new Error('rolled back');
          }),
        ).rejects.toThrow('rolled back');
      }),
    ).toBe(0);
  });

  it('counts a withdrawal transition from the status it actually left', async () => {
    const account = await helpers.createTestAccount(pool);
    await helpers.fundTestAccount(pool, account, 1000n);
    const withdrawal = await helpers.createTestWithdrawal(pool, account, 400n);
    expect(
      await counted({ entity: 'withdrawal', from: 'held', to: 'submitted' }, () =>
        withTransaction(pool, (tx) => markSubmitted(tx, withdrawal.id, 'tr_1')),
      ),
    ).toBe(1);
  });
});
