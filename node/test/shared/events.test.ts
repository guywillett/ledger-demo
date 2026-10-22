import { describe, expect, it } from 'vitest';
import { paymentCreatedV1Schema } from '../../src/shared/events.ts';
import { expectEventMatchesContract } from '../contract.ts';

// The Zod schema (what Node consumers use) and the hand-written JSON Schema in contracts/events/
// (what Go will use) must agree. Each case must be accepted by both or rejected by both.

const valid = {
  event_id: 'evt_01m5cmffcafzwacj4r9sfjgy3z',
  event_type: 'payment.created',
  schema_version: 1,
  payment_id: 'pay_01m5cmffajfzwacj4hc9tw3ekg',
  network: 'SEPA',
  network_transaction_id: 'NTX-1',
  account_id: 'acct_01m564xr00fx7rjvqryn5dzwxx',
  amount_minor: '1250',
  currency: 'EUR',
  occurred_at: '2026-10-20T12:27:16.213Z',
};

function acceptedByContract(event: unknown): boolean {
  try {
    expectEventMatchesContract(event);
    return true;
  } catch {
    return false;
  }
}

describe('payment.created v1 schemas agree', () => {
  it('both accept a valid event, and ignore unknown fields', () => {
    for (const event of [valid, { ...valid, added_later: 'x' }]) {
      expect(paymentCreatedV1Schema.safeParse(event).success).toBe(true);
      expect(acceptedByContract(event)).toBe(true);
    }
  });

  it.each([
    ['amount above 19 digits', { amount_minor: '1'.repeat(20) }],
    ['amount with a leading zero', { amount_minor: '0125' }],
    ['numeric amount', { amount_minor: 1250 }],
    ['wrong ID prefix', { payment_id: 'acct_01m564xr00fx7rjvqryn5dzwxx' }],
    ['other currency', { currency: 'USD' }],
    ['other schema version', { schema_version: 2 }],
    ['empty network transaction ID', { network_transaction_id: '' }],
    ['timestamp that is not ISO 8601', { occurred_at: '20 October 2026' }],
    ['missing account_id', { account_id: undefined }],
  ])('both reject: %s', (_label, overrides) => {
    const event = JSON.parse(JSON.stringify({ ...valid, ...overrides })) as unknown;
    expect(paymentCreatedV1Schema.safeParse(event).success).toBe(false);
    expect(acceptedByContract(event)).toBe(false);
  });
});
