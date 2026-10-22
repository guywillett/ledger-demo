import { z } from 'zod';
import { parseIban, type Iban } from '../shared/iban.ts';
import { toTypeId, type TypeIdString } from '../shared/ids.ts';
import { amountMinorSchema, typeIdSchema } from '../shared/schemas.ts';
import type { Notification } from '../ingestion/notifications.ts';
import type { Payment } from '../payments/payments.ts';
import type { Balance } from '../ledger/balances.ts';
import type { PaymentStatus, StatusChange } from '../payments/status.ts';

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
  amount_minor: amountMinorSchema,
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

export const accountParamsSchema = z.object({ account_id: typeIdSchema('acct') });

// Unknown query parameters are ignored (z.object strips them), like unknown body fields.
// z.coerce: query parameters arrive as strings.
export const transactionsQuerySchema = z.object({
  include: z.literal('history').optional(),
  limit: z.coerce.number().int().min(1).max(200).default(50),
  before: typeIdSchema('pay').optional(),
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
