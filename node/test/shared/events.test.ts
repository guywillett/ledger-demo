import { describe, expect, it } from 'vitest';
import { paymentCreatedV1Schema, withdrawalHeldV1Schema } from '../../src/shared/events.ts';
import { toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { expectEventMatchesContract } from '../contract.ts';

// The Zod schema (what Node consumers use) and the hand-written JSON Schema in contracts/events/
// (what Go will use) must agree. Each case must be accepted by both or rejected by both, apart
// from the deliberate exceptions at the end.

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

// Where Zod is deliberately stricter than the JSON contract, because JSON Schema can't express
// the rule (see src/shared/schemas.ts): the contract accepts these, Zod rejects them.
describe('payment.created v1: Zod stricter than the contract', () => {
  it.each([
    [
      'a TypeID holding a non-v7 UUID',
      { payment_id: toTypeId('pay', '2c5ea4c0-4067-41fb-9c1e-5c3f2d7e8a90' as Uuid<'pay'>) },
    ],
    ['an amount above the int8 maximum', { amount_minor: '9223372036854775808' }],
  ])('only Zod rejects %s', (_label, overrides) => {
    const event = { ...valid, ...overrides };
    expect(acceptedByContract(event)).toBe(true);
    expect(paymentCreatedV1Schema.safeParse(event).success).toBe(false);
  });
});

const validHeld = {
  event_id: 'evt_01m5cmffcafzwacj4r9sfjgy3z',
  event_type: 'withdrawal.held',
  schema_version: 1,
  withdrawal_id: 'wd_01m5cmffajfzwacj4hc9tw3ekg',
  account_id: 'acct_01m564xr00fx7rjvqryn5dzwxx',
  amount_minor: '2500',
  currency: 'EUR',
  occurred_at: '2026-10-22T12:27:16.213Z',
};

describe('withdrawal.held v1 schemas agree', () => {
  it('both accept a valid event, and ignore unknown fields', () => {
    for (const event of [validHeld, { ...validHeld, added_later: 'x' }]) {
      expect(withdrawalHeldV1Schema.safeParse(event).success).toBe(true);
      expect(acceptedByContract(event)).toBe(true);
    }
  });

  it.each([
    ['a payment ID as the withdrawal', { withdrawal_id: 'pay_01m5cmffajfzwacj4hc9tw3ekg' }],
    ['numeric amount', { amount_minor: 2500 }],
    ['zero amount', { amount_minor: '0' }],
    ['other currency', { currency: 'USD' }],
    ['other event type', { event_type: 'withdrawal.executed' }],
    ['other schema version', { schema_version: 2 }],
    ['missing account_id', { account_id: undefined }],
    ['timestamp that is not ISO 8601', { occurred_at: '22 October 2026' }],
  ])('both reject: %s', (_label, overrides) => {
    const event = JSON.parse(JSON.stringify({ ...validHeld, ...overrides })) as unknown;
    expect(withdrawalHeldV1Schema.safeParse(event).success).toBe(false);
    expect(acceptedByContract(event)).toBe(false);
  });

  it('carries no personal data: no IBAN, creditor name or remittance text', () => {
    const fields = Object.keys(withdrawalHeldV1Schema.shape);
    for (const field of ['creditor_iban', 'creditor_name', 'remittance_info']) {
      expect(fields).not.toContain(field);
    }
  });
});
