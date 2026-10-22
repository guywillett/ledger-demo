import { Writable } from 'node:stream';
import { describe, expect, it } from 'vitest';
import { createLogger, stringifyBigInts } from '../../src/shared/logger.ts';

describe('stringifyBigInts', () => {
  it('turns BigInt values into digit strings, including nested ones', () => {
    expect(
      stringifyBigInts({
        amount_minor: 9007199254740993n,
        payment: { amount_minor: -5n, items: [1n, 'x', { n: 2n }] },
        count: 3,
      }),
    ).toEqual({
      amount_minor: '9007199254740993',
      payment: { amount_minor: '-5', items: ['1', 'x', { n: '2' }] },
      count: 3,
    });
  });

  it('leaves errors and other class instances to pino', () => {
    const err = new Error('boom');
    const at = new Date();
    const result = stringifyBigInts({ err, at });
    expect(result.err).toBe(err);
    expect(result.at).toBe(at);
  });

  it('converts an object that appears more than once', () => {
    const shared = { amount_minor: 1250n };
    expect(stringifyBigInts({ before: shared, after: [shared] })).toEqual({
      before: { amount_minor: '1250' },
      after: [{ amount_minor: '1250' }],
    });
  });

  it('keeps circular references circular in the converted copy', () => {
    const object: Record<string, unknown> = { n: 1n };
    object.self = object;
    const result = stringifyBigInts(object);
    expect(result.n).toBe('1');
    expect(result.self).toBe(result);
  });

  it('does not modify its input', () => {
    const input = { amount_minor: 5n };
    stringifyBigInts(input);
    expect(input.amount_minor).toBe(5n);
  });
});

describe('createLogger', () => {
  // Collects the JSON lines the logger writes, instead of sending them to stdout.
  function capture(): { destination: Writable; lines: () => Record<string, unknown>[] } {
    const chunks: string[] = [];
    const destination = new Writable({
      write(chunk, _encoding, callback) {
        chunks.push(String(chunk));
        callback();
      },
    });
    const lines = () =>
      chunks
        .join('')
        .trim()
        .split('\n')
        .map((line) => JSON.parse(line) as Record<string, unknown>);
    return { destination, lines };
  }

  it('writes BigInt properties of a logged error as digit strings', () => {
    const { destination, lines } = capture();
    const logger = createLogger({ service: 'test', level: 'info' }, destination);
    const err = Object.assign(new Error('boom'), { amountMinor: 9007199254740993n });

    logger.error({ err }, 'probe');

    expect(lines()).toEqual([
      expect.objectContaining({
        err: expect.objectContaining({ message: 'boom', amountMinor: '9007199254740993' }),
      }),
    ]);
  });

  it('writes BigInt values in log objects as digit strings', () => {
    const { destination, lines } = capture();
    const logger = createLogger({ service: 'test', level: 'info' }, destination);

    logger.child({ payment_id: 'pay_x' }).info({ amount_minor: 9007199254740993n }, 'probe');

    expect(lines()).toEqual([
      expect.objectContaining({
        level: 'info',
        service: 'test',
        payment_id: 'pay_x',
        amount_minor: '9007199254740993',
        msg: 'probe',
      }),
    ]);
  });
});
