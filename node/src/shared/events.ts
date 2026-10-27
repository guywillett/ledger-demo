import { z } from 'zod';
import { amountMinorSchema, typeIdSchema } from './schemas.ts';

// Event contracts, shared by producers and consumers. The language-neutral source is
// contracts/events/*.json; these Zod schemas must match them (tests check both), except
// where Zod is stricter than JSON Schema can be (see shared/schemas.ts).
// Events carry our IDs only (TypeIDs), never provider identifiers such as IBANs.

export const SUBJECTS = {
  depositCreated: 'deposit.created',
  withdrawalHeld: 'withdrawal.held',
} as const;

// One schema, two types. z.input is the wire format a producer builds (strings, TypeIDs);
// z.output is what a consumer gets from parse() (our UUIDs, a BigInt amount, a Date).
//
// z.object (not strictObject): consumers ignore unknown fields, so a producer can add optional
// fields within a version.
export const depositCreatedV1Schema = z.object({
  event_id: typeIdSchema('evt'),
  event_type: z.literal('deposit.created'),
  schema_version: z.literal(1),
  deposit_id: typeIdSchema('dep'),
  network: z.literal('SEPA'),
  network_transaction_id: z.string().min(1).max(255),
  account_id: typeIdSchema('acct'),
  amount_minor: amountMinorSchema,
  currency: z.literal('EUR'),
  occurred_at: z.iso.datetime().transform((value) => new Date(value)),
});

// What goes on the wire (producers build this).
export type DepositCreatedV1Wire = z.input<typeof depositCreatedV1Schema>;
// What a consumer works with after parsing.
export type DepositCreatedV1 = z.output<typeof depositCreatedV1Schema>;

// withdrawal.held: a withdrawal was created and its funds held. Deliberately without the
// destination IBAN, the creditor name or the remittance text: those are personal data, and a
// stream keeps messages (7 days here) beyond the database's control. As slice 1's events carry
// our IDs only, consumers (the submitter) read the details from the database by withdrawal_id.
export const withdrawalHeldV1Schema = z.object({
  event_id: typeIdSchema('evt'),
  event_type: z.literal('withdrawal.held'),
  schema_version: z.literal(1),
  withdrawal_id: typeIdSchema('wd'),
  account_id: typeIdSchema('acct'),
  amount_minor: amountMinorSchema,
  currency: z.literal('EUR'),
  occurred_at: z.iso.datetime().transform((value) => new Date(value)),
});

export type WithdrawalHeldV1Wire = z.input<typeof withdrawalHeldV1Schema>;
export type WithdrawalHeldV1 = z.output<typeof withdrawalHeldV1Schema>;
