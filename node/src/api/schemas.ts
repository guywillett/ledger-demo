import { z } from 'zod';
import { parseIban, type Iban } from '../shared/iban.ts';
import {
  fromTypeId,
  toTypeId,
  type IdPrefix,
  type TypeIdString,
  type Uuid,
} from '../shared/ids.ts';
import type { Notification, Payment } from '../ingestion/payments.ts';
import type { Balance } from '../ledger/balances.ts';
import type { PaymentStatus, StatusChange } from '../shared/payment-status.ts';

const INT8_MAX = 9223372036854775807n;

// POST /payments body. z.object (not strictObject) drops unknown fields instead of rejecting
// them: providers add fields to webhooks over time ("tolerant reader"). Dropped fields are
// neither stored nor logged.
export const notificationBodySchema = z.object({
  network_transaction_id: z.string().min(1).max(255),
  creditor_iban: z
    .string()
    .max(64)
    .transform((value, ctx): Iban => {
      const result = parseIban(value);
      if (!result.ok) {
        ctx.issues.push({
          code: 'custom',
          message: `invalid IBAN: ${result.reason}`,
          input: value,
        });
        return z.NEVER;
      }
      return result.iban;
    }),
  provider_account_id: z.string().min(1).max(255).optional(),
  // Digits only, no leading zero (so "0125" is not a second spelling of 125), at most int8.
  amount_minor: z
    .string()
    .max(19)
    .regex(/^[1-9][0-9]*$/, 'must be a positive whole number of minor units, as a string')
    .transform((value) => BigInt(value))
    .refine((value) => value <= INT8_MAX, 'is larger than the maximum amount'),
  currency: z.literal('EUR'),
  network: z.literal('SEPA'),
});

export function toNotification(body: z.infer<typeof notificationBodySchema>): Notification {
  return {
    networkTransactionId: body.network_transaction_id,
    creditorIban: body.creditor_iban,
    providerAccountId: body.provider_account_id ?? null,
    amountMinor: body.amount_minor,
    currency: body.currency,
    network: body.network,
  };
}

export type PaymentResponse = {
  readonly id: TypeIdString<'pay'>;
  readonly account_id: TypeIdString<'acct'>;
  readonly creditor_iban: string;
  readonly provider_account_id: string | null;
  readonly amount_minor: string;
  readonly currency: 'EUR';
  readonly network: 'SEPA';
  readonly network_transaction_id: string;
  readonly status: PaymentStatus;
  readonly notification_count: number;
  readonly created_at: string;
  readonly updated_at: string;
};

// Explicit mapping to the wire format: TypeIDs instead of UUIDs, amounts as digit strings
// (JSON.stringify would throw on a BigInt), ISO 8601 timestamps.
export function toPaymentResponse(payment: Payment): PaymentResponse {
  return {
    id: toTypeId('pay', payment.id),
    account_id: toTypeId('acct', payment.accountId),
    creditor_iban: payment.creditorIban,
    provider_account_id: payment.providerAccountId,
    amount_minor: payment.amountMinor.toString(),
    currency: payment.currency,
    network: payment.network,
    network_transaction_id: payment.networkTransactionId,
    status: payment.status,
    notification_count: payment.notificationCount,
    created_at: payment.createdAt.toISOString(),
    updated_at: payment.updatedAt.toISOString(),
  };
}

// A TypeID in a path or query parameter, parsed into our UUID (checking the prefix and that it
// holds a UUIDv7). The message doesn't repeat the input.
const typeIdParam = <P extends IdPrefix>(prefix: P): z.ZodType<Uuid<P>, string> =>
  z.string().transform((value, ctx): Uuid<P> => {
    try {
      return fromTypeId(prefix, value);
    } catch {
      ctx.issues.push({ code: 'custom', message: `must be a ${prefix}_ ID`, input: value });
      return z.NEVER;
    }
  });

export const accountParamsSchema = z.object({ account_id: typeIdParam('acct') });

// Unknown query parameters are ignored (z.object strips them), like unknown body fields.
// z.coerce: query parameters arrive as strings.
export const transactionsQuerySchema = z.object({
  include: z.literal('history').optional(),
  limit: z.coerce.number().int().min(1).max(200).default(50),
  before: typeIdParam('pay').optional(),
});

export type BalanceResponse = {
  readonly pending_in: string;
  readonly available: string;
  readonly currency: 'EUR';
};

export function toBalanceResponse(balance: Balance): BalanceResponse {
  return {
    pending_in: balance.pendingIn.toString(),
    available: balance.available.toString(),
    currency: balance.currency,
  };
}

export type StatusChangeResponse = {
  readonly from_status: PaymentStatus | null;
  readonly to_status: PaymentStatus;
  readonly reason: string | null;
  readonly at: string;
};

export type TransactionResponse = PaymentResponse & {
  readonly history?: readonly StatusChangeResponse[];
};

export type TransactionListResponse = {
  readonly payments: readonly TransactionResponse[];
  readonly next_before: TypeIdString<'pay'> | null;
};

export function toStatusChangeResponse(change: StatusChange): StatusChangeResponse {
  return {
    from_status: change.from,
    to_status: change.to,
    reason: change.reason,
    at: change.at.toISOString(),
  };
}
