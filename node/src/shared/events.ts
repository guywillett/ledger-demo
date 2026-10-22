import { z } from 'zod';
import { amountMinorSchema, typeIdSchema } from './schemas.ts';

// Event contracts, shared by producers and consumers. The language-neutral source is
// contracts/events/*.json; these Zod schemas must match them (tests check both), except
// where Zod is stricter than JSON Schema can be (see shared/schemas.ts).
// Events carry our IDs only (TypeIDs), never provider identifiers such as IBANs.

export const SUBJECTS = {
  paymentCreated: 'payment.created',
} as const;

// One schema, two types. z.input is the wire format a producer builds (strings, TypeIDs);
// z.output is what a consumer gets from parse() (our UUIDs, a BigInt amount, a Date).
//
// z.object (not strictObject): consumers ignore unknown fields, so a producer can add optional
// fields within a version.
export const paymentCreatedV1Schema = z.object({
  event_id: typeIdSchema('evt'),
  event_type: z.literal('payment.created'),
  schema_version: z.literal(1),
  payment_id: typeIdSchema('pay'),
  network: z.literal('SEPA'),
  network_transaction_id: z.string().min(1).max(255),
  account_id: typeIdSchema('acct'),
  amount_minor: amountMinorSchema,
  currency: z.literal('EUR'),
  occurred_at: z.iso.datetime().transform((value) => new Date(value)),
});

// What goes on the wire (producers build this).
export type PaymentCreatedV1Wire = z.input<typeof paymentCreatedV1Schema>;
// What a consumer works with after parsing.
export type PaymentCreatedV1 = z.output<typeof paymentCreatedV1Schema>;
