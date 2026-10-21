import { z } from 'zod';

// Event contracts, shared by producers and consumers. The language-neutral source is
// contracts/events/*.json; these Zod schemas must match them (tests check both).
// Events carry our IDs only (TypeIDs), never provider identifiers such as IBANs.

export const SUBJECTS = {
  paymentCreated: 'payment.created',
} as const;

const typeId = (prefix: string): z.ZodString =>
  z.string().regex(new RegExp(`^${prefix}_[0-7][0-9a-hjkmnp-tv-z]{25}$`));

// z.object (not strictObject): consumers ignore unknown fields, so a producer can add optional
// fields within a version.
export const paymentCreatedV1Schema = z.object({
  event_id: typeId('evt'),
  event_type: z.literal('payment.created'),
  schema_version: z.literal(1),
  payment_id: typeId('pay'),
  network: z.literal('SEPA'),
  network_transaction_id: z.string().min(1).max(255),
  account_id: typeId('acct'),
  amount_minor: z
    .string()
    .max(19)
    .regex(/^[1-9][0-9]*$/),
  currency: z.literal('EUR'),
  occurred_at: z.iso.datetime(),
});

export type PaymentCreatedV1 = z.infer<typeof paymentCreatedV1Schema>;
