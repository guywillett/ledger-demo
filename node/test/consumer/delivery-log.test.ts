import { afterAll, describe, expect, it } from 'vitest';
import { z } from 'zod';
import { withDeliveryLog, type DeliveryLogOptions } from '../../src/consumer/delivery-log.ts';
import type { DeliveredMessage } from '../../src/consumer/messages.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';
import { capturingLogger, createTestPool } from '../helpers.ts';

// A handler must settle the message on every path, and only once. The first test is
// compile-time only: Settled is created only
// by the context's done(), stop() and retryLater(), so these must not compile. tsc (run first
// by npm test) is the assertion; nothing here is executed.

declare const options: DeliveryLogOptions<z.ZodObject<{ n: z.ZodNumber }>>;
declare const msg: DeliveredMessage;

describe('withDeliveryLog types', () => {
  it('rejects a handler path that never settles the message, and a forged Settled', () => {
    const check = (): void => {
      // @ts-expect-error a path returns without ack, term or nak
      void withDeliveryLog(options, msg, async (event, ctx) => {
        if (event.n > 0) return ctx.done('applied');
      });
      // @ts-expect-error a plain object is not a Settled
      void withDeliveryLog(options, msg, async () => ({ outcome: 'applied' as const }));
      // Every path settled: compiles.
      void withDeliveryLog(options, msg, async (event, ctx) =>
        event.n > 0 ? ctx.done('applied') : ctx.stop('invalid', 'not positive'),
      );
    };
    expect(typeof check).toBe('function');
  });
});

describe('withDeliveryLog', () => {
  const pool = createTestPool();
  afterAll(() => pool.end());

  it('settles a message once: a second settle throws, and nothing is signalled twice', async () => {
    const calls: string[] = [];
    const fake: DeliveredMessage = {
      // An event_id: the delivery log records a null one only for invalid messages.
      body: JSON.stringify({ event_id: toTypeId('evt', newUuid('evt')), n: 1 }),
      natsMsgId: 'double',
      deliveryAttempt: 1,
      ack: () => calls.push('ack'),
      term: () => calls.push('term'),
      nak: () => calls.push('nak'),
      working: () => calls.push('working'),
    };
    const { logger, lines } = capturingLogger();
    const outcome = await withDeliveryLog(
      {
        consumer: 'worker',
        eventType: 'test.event',
        schema: z.object({ n: z.number() }),
        pool,
        logger,
        retry: { retryBaseDelayMs: 1000, retryMaxDelayMs: 1000 },
        isBug: () => false,
      },
      fake,
      async (_event, ctx) => {
        await ctx.done('applied');
        return ctx.stop('failed', 'too late');
      },
    );
    expect(outcome).toBe('applied');
    expect(calls).toEqual(['ack']);
    expect(lines().find((l) => l.level === 'error')).toMatchObject({
      msg: 'error after the message was settled; not settling it again',
    });
  });
});
