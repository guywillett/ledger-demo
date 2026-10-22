import { z } from 'zod';
import { toTypeId, type TypeIdString } from '../shared/ids.ts';
import {
  amountMinorSchema,
  ibanSchema,
  optionalSepaTextSchema,
  sepaTextSchema,
  typeIdSchema,
} from '../shared/schemas.ts';
import type { Notification } from '../ingestion/notifications.ts';
import type { Payment } from '../payments/payments.ts';
import type { Balance } from '../ledger/balances.ts';
import type { PaymentStatus, StatusChange } from '../payments/status.ts';
import type { WithdrawalStatus, WithdrawalStatusChange } from '../withdrawals/status.ts';
import type { NewWithdrawal, Withdrawal } from '../withdrawals/withdrawals.ts';
import type { Uuid } from '../shared/ids.ts';

// POST /payments body. z.object (not strictObject) drops unknown fields instead of rejecting
// them: providers add fields to webhooks over time ("tolerant reader"). Dropped fields are
// neither stored nor logged. Client requests are the opposite (withdrawalBodySchema, slice 2
// decision 33).
export const notificationBodySchema = z.object({
  network_transaction_id: z.string().min(1).max(255),
  creditor_iban: ibanSchema,
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
  readonly held: string;
  readonly currency: 'EUR';
};

export function toBalanceResponse(balance: Balance): BalanceResponse {
  return {
    pending_in: balance.pendingIn.toString(),
    available: balance.available.toString(),
    held: balance.held.toString(),
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

// The Idempotency-Key header of POST /accounts/:account_id/withdrawals: required, 1-255
// characters from a plain set (it ends up in logs). Fastify lowercases header names; a header
// sent twice arrives as an array, which fails the string check.
export const idempotencyKeyHeadersSchema = z.object({
  'idempotency-key': z
    .string({ error: 'is required' })
    .regex(/^[A-Za-z0-9._:-]{1,255}$/, 'must be 1-255 characters from A-Z a-z 0-9 . _ : -'),
});

// POST /accounts/:account_id/withdrawals body. z.strictObject: unknown fields are rejected, the
// opposite of the provider webhook (notificationBodySchema). This is our own client's request
// to move money, so a misspelt optional field (remitance_info) must fail rather than be
// silently dropped (slice 2 decision 33).
export const withdrawalBodySchema = z.strictObject({
  amount_minor: amountMinorSchema,
  currency: z.literal('EUR'),
  creditor_iban: ibanSchema,
  // SEPA: required, at most 70 characters, the basic character set (decision 32).
  creditor_name: sepaTextSchema(70),
  // SEPA unstructured remittance information: optional, at most 140 characters.
  remittance_info: optionalSepaTextSchema(140),
});

export function toNewWithdrawal(
  accountId: Uuid<'acct'>,
  idempotencyKey: string,
  body: z.infer<typeof withdrawalBodySchema>,
): NewWithdrawal {
  return {
    accountId,
    idempotencyKey,
    amountMinor: body.amount_minor,
    currency: body.currency,
    creditorIban: body.creditor_iban,
    creditorName: body.creditor_name,
    remittanceInfo: body.remittance_info,
  };
}

export const withdrawalParamsSchema = z.object({ withdrawal_id: typeIdSchema('wd') });

export const withdrawalQuerySchema = z.object({ include: z.literal('history').optional() });

export type WithdrawalStatusChangeResponse = {
  readonly from_status: WithdrawalStatus | null;
  readonly to_status: WithdrawalStatus;
  readonly reason: string | null;
  readonly at: string;
};

// No provider_transfer_id: provider identifiers stay at the edge, as for payments' events.
export type WithdrawalResponse = {
  readonly id: TypeIdString<'wd'>;
  readonly account_id: TypeIdString<'acct'>;
  readonly idempotency_key: string;
  readonly amount_minor: string;
  readonly currency: 'EUR';
  readonly creditor_iban: string;
  readonly creditor_name: string;
  readonly remittance_info: string | null;
  readonly status: WithdrawalStatus;
  readonly failure_reason: string | null;
  readonly created_at: string;
  readonly updated_at: string;
  readonly history?: readonly WithdrawalStatusChangeResponse[];
};

export function toWithdrawalResponse(withdrawal: Withdrawal): WithdrawalResponse {
  return {
    id: toTypeId('wd', withdrawal.id),
    account_id: toTypeId('acct', withdrawal.accountId),
    idempotency_key: withdrawal.idempotencyKey,
    amount_minor: withdrawal.amountMinor.toString(),
    currency: withdrawal.currency,
    creditor_iban: withdrawal.creditorIban,
    creditor_name: withdrawal.creditorName,
    remittance_info: withdrawal.remittanceInfo,
    status: withdrawal.status,
    failure_reason: withdrawal.failureReason,
    created_at: withdrawal.createdAt.toISOString(),
    updated_at: withdrawal.updatedAt.toISOString(),
  };
}

export function toWithdrawalStatusChangeResponse(
  change: WithdrawalStatusChange,
): WithdrawalStatusChangeResponse {
  return {
    from_status: change.from,
    to_status: change.to,
    reason: change.reason,
    at: change.at.toISOString(),
  };
}
