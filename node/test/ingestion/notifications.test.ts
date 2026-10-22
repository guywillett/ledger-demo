import { describe, expect, it } from 'vitest';
import { type Notification, recordNotification } from '../../src/ingestion/notifications.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { parseIban, type Iban } from '../../src/shared/iban.ts';
import { refusingPool } from '../helpers.ts';

// Unit tests for the classification after a refused notification. The inconsistent case can't
// be produced against the real database without changing shared seed data, so these use a stub
// pool that returns canned rows (refusingPool in helpers.ts).

function iban(value: string): Iban {
  const result = parseIban(value);
  if (!result.ok) throw new Error(`test IBAN ${value} is invalid: ${result.reason}`);
  return result.iban;
}

const ALICE_IBAN = iban('FR7612345000010000000000193');

function notification(overrides: Partial<Notification> = {}): Notification {
  return {
    creditorIban: ALICE_IBAN,
    providerAccountId: null,
    amountMinor: 1250n,
    currency: 'EUR',
    network: 'SEPA',
    networkTransactionId: 'NTX-STUB',
    ...overrides,
  };
}

describe('recordNotification after a refused notification', () => {
  it('reports a conflict naming the fields that differ', async () => {
    const pool = refusingPool({ creditor_iban: ALICE_IBAN, amount_minor: 999n, currency: 'EUR' });
    const result = await withTransaction(pool, (tx) => recordNotification(tx, notification()));
    expect(result.kind).toBe('conflict');
    expect(result.kind === 'conflict' && result.differingFields).toEqual(['amount_minor']);
  });

  it('reports an inconsistency, not a conflict, when no compared field differs', async () => {
    const pool = refusingPool({ creditor_iban: ALICE_IBAN, amount_minor: 1250n, currency: 'EUR' });
    const result = await withTransaction(pool, (tx) => recordNotification(tx, notification()));
    expect(result.kind).toBe('inconsistent');
  });
});
