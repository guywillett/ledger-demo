import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import type { z } from 'zod';
import { toNewWithdrawal, withdrawalBodySchema } from '../src/api/schemas.ts';
import { notificationBodySchema, toNotification } from '../src/api/webhooks/schemas.ts';
import { differingDepositFields } from '../src/ingestion/incoming-payments.ts';
import type { IdPrefix, Uuid } from '../src/shared/ids.ts';
import {
  amountMinorSchema,
  ibanSchema,
  optionalSepaTextSchema,
  sepaTextSchema,
  typeIdSchema,
} from '../src/shared/schemas.ts';
import { differingWithdrawalFields } from '../src/withdrawals/requests.ts';
import { contractPropertyErrors } from './contract.ts';

// The shared test vectors (contracts/vectors/, see its README): every case through the real
// boundary code. Node is the reference implementation, so a failure here means Node's behaviour
// changed, and Go would have to change with it (or the vector was wrong).

type Case = {
  readonly name: string;
  readonly input: unknown;
  readonly ok: boolean;
  readonly value?: unknown;
  readonly why?: string;
};

function load(file: string): Case[] {
  const path = join(import.meta.dirname, '../../contracts/vectors', file);
  return JSON.parse(readFileSync(path, 'utf8')) as Case[];
}

type Outcome = { ok: true; value: unknown } | { ok: false };

// Runs input through schema; value is the parsed result, made JSON-comparable by toValue.
function viaSchema(
  schema: z.ZodType,
  input: unknown,
  toValue: (value: unknown) => unknown = (value) => value,
): Outcome {
  const result = schema.safeParse(input);
  return result.success ? { ok: true, value: toValue(result.data) } : { ok: false };
}

function check(file: string, evaluate: (input: unknown) => Outcome): void {
  describe(file, () => {
    it.each(load(file).map((c) => [c.name, c] as const))('%s', (_name, c) => {
      const outcome = evaluate(c.input);
      expect(outcome).toEqual(c.ok ? { ok: true, value: c.value } : { ok: false });
    });
  });
}

check('money.json', (input) => viaSchema(amountMinorSchema, input, String));

check('typeid.json', (input) => {
  const { prefix, text } = input as { prefix: IdPrefix; text: unknown };
  return viaSchema(typeIdSchema(prefix), text);
});

check('iban.json', (input) => viaSchema(ibanSchema, input));

check('sepa-text.json', (input) => {
  const { max, optional, text } = input as { max: number; optional: boolean; text?: unknown };
  return viaSchema(optional ? optionalSepaTextSchema(max) : sepaTextSchema(max), text);
});

const ACCOUNT = '01a14c4e-e000-7f4f-896e-f8f54adff3bd' as Uuid<'acct'>;

check('withdrawal-same-details.json', (input) => {
  const { a, b } = input as { a: unknown; b: unknown };
  const parse = (body: unknown) =>
    toNewWithdrawal(ACCOUNT, 'key', withdrawalBodySchema.parse(body));
  return { ok: true, value: differingWithdrawalFields(parse(a), parse(b)) };
});

check('deposit-same-details.json', (input) => {
  const { a, b } = input as { a: unknown; b: unknown };
  const parse = (body: unknown) => toNotification(notificationBodySchema.parse(body));
  return { ok: true, value: differingDepositFields(parse(a), parse(b)) };
});

// One way only: every value the implementation accepts must be valid against the contract's
// request schema for that field. Not the reverse: the contract can't express a checksum, a
// country's IBAN length or a length after trimming, so it accepts some values the
// implementation rejects.
describe('accepted inputs are valid against the contract', () => {
  const accepted = (file: string) => load(file).filter((c) => c.ok);

  it.each(accepted('iban.json').map((c) => [c.name, c] as const))('iban: %s', (_name, c) => {
    expect(contractPropertyErrors('WithdrawalRequest', 'creditor_iban', c.input)).toBeUndefined();
  });

  it.each(accepted('sepa-text.json').map((c) => [c.name, c] as const))(
    'sepa-text: %s',
    (_name, c) => {
      const { optional, text } = c.input as { optional: boolean; text?: unknown };
      if (text === undefined) return; // an absent field: nothing to validate
      const property = optional ? 'remittance_info' : 'creditor_name';
      expect(contractPropertyErrors('WithdrawalRequest', property, text)).toBeUndefined();
    },
  );
});
