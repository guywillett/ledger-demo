import { z } from 'zod';
import { toTypeId, type TypeIdString } from '../shared/ids.ts';
import {
  ibanSchema,
  optionalSepaTextSchema,
  sepaAmountSchema,
  sepaTextSchema,
  typeIdSchema,
} from '../shared/schemas.ts';
import type { Account } from '../accounts/accounts.ts';
import type { Deposit } from '../deposits/deposits.ts';
import type { Balance } from '../ledger/balances.ts';
import { BOOKED_STATUSES, type DepositStatus, type StatusChange } from '../deposits/status.ts';
import {
  WITHDRAWAL_STATUSES,
  type WithdrawalStatus,
  type WithdrawalStatusChange,
} from '../withdrawals/status.ts';
import type { NewWithdrawal, Withdrawal } from '../withdrawals/withdrawals.ts';
import type { Uuid } from '../shared/ids.ts';

// A deposit's history row as a client sees it. reason is always null: the only reasons a
// deposit's history holds are the compliance decision's, which are internal (slice 4 decision
// 76: telling a customer why a payment was rejected can be tipping off). Hidden here, at the
// boundary, rather than by transition: a reason added to some other transition later stays
// hidden until it is deliberately made client-facing.
export type DepositStatusChangeResponse = {
  readonly from_status: DepositStatus | null;
  readonly to_status: DepositStatus;
  readonly reason: null;
  readonly at: string;
};

export function toDepositStatusChangeResponse(change: StatusChange): DepositStatusChangeResponse {
  return {
    from_status: change.from,
    to_status: change.to,
    reason: null,
    at: change.at.toISOString(),
  };
}

// The one client-facing reason for a deposit that failed (slice 4 decisions 69 and 76): the
// same fixed, non-disclosing text for rejected and returned, whatever the internal reason. A
// rejected deposit is returned next (the ledger's return posting), so the text is true of both.
export const DEPOSIT_FAILURE_REASON = 'returned to the sender';

function depositFailureReason(status: DepositStatus): string | null {
  return status === 'rejected' || status === 'returned' ? DEPOSIT_FAILURE_REASON : null;
}

// type first (decision 67), then the deposit's fields; no provider_account_id, a provider
// identifier (decision 69). history only with include=history.
export type DepositResponse = {
  readonly type: 'deposit';
  readonly id: TypeIdString<'dep'>;
  readonly account_id: TypeIdString<'acct'>;
  readonly creditor_iban: string;
  readonly amount_minor: string;
  readonly currency: 'EUR';
  readonly network: 'SEPA';
  readonly network_transaction_id: string;
  readonly status: DepositStatus;
  readonly failure_reason: string | null;
  readonly notification_count: number;
  readonly created_at: string;
  readonly updated_at: string;
  readonly history?: readonly DepositStatusChangeResponse[];
};

// Explicit mapping to the wire format: TypeIDs instead of UUIDs, amounts as digit strings
// (JSON.stringify would throw on a BigInt), ISO 8601 timestamps. With history when given (an
// array, empty if the deposit has none).
export function toDepositResponse(
  deposit: Deposit,
  history?: readonly StatusChange[],
): DepositResponse {
  return {
    type: 'deposit',
    id: toTypeId('dep', deposit.id),
    account_id: toTypeId('acct', deposit.accountId),
    creditor_iban: deposit.creditorIban,
    amount_minor: deposit.amountMinor.toString(),
    currency: deposit.currency,
    network: deposit.network,
    network_transaction_id: deposit.networkTransactionId,
    status: deposit.status,
    failure_reason: depositFailureReason(deposit.status),
    notification_count: deposit.notificationCount,
    created_at: deposit.createdAt.toISOString(),
    updated_at: deposit.updatedAt.toISOString(),
    ...(history === undefined ? {} : { history: history.map(toDepositStatusChangeResponse) }),
  };
}

export type DepositListResponse = {
  readonly data: readonly DepositResponse[];
  readonly next_before: TypeIdString<'dep'> | null;
};

export const accountParamsSchema = z.object({ account_id: typeIdSchema('acct') });

// Unknown query parameters are ignored (z.object strips them), like unknown body fields.
// z.coerce: query parameters arrive as strings. The keys are in the contract's parameter order:
// Zod reports problems in the order of the shape's keys, and the API reports them in the
// contract's order (slice 4 decision 68).
export const depositsQuerySchema = z.object({
  limit: z.coerce.number().int().min(1).max(200).default(50),
  before: typeIdSchema('dep').optional(),
  include: z.literal('history').optional(),
  // The filters (slice 4 decision 70). status: one an account's deposit can have (not unmatched).
  // network_transaction_id: as the incoming-payment webhook accepts it (webhooks/schemas.ts), so
  // a value no deposit could have is a 400 rather than an empty list.
  status: z.enum(BOOKED_STATUSES).optional(),
  network_transaction_id: z.string().min(1).max(255).optional(),
});

// GET /accounts/:account_id/deposits/:deposit_id: both IDs, in the contract's order.
export const depositParamsSchema = z.object({
  account_id: typeIdSchema('acct'),
  deposit_id: typeIdSchema('dep'),
});

export const depositQuerySchema = z.object({ include: z.literal('history').optional() });

export type AccountResponse = {
  readonly id: TypeIdString<'acct'>;
  readonly name: string;
  readonly iban: string;
  readonly created_at: string;
};

export function toAccountResponse(account: Account): AccountResponse {
  return {
    id: toTypeId('acct', account.id),
    name: account.name,
    iban: account.iban,
    created_at: account.createdAt.toISOString(),
  };
}

export type BalanceResponse = {
  readonly account_id: TypeIdString<'acct'>;
  readonly pending_in: string;
  readonly available: string;
  readonly held: string;
  readonly currency: 'EUR';
};

export function toBalanceResponse(balance: Balance, accountId: Uuid<'acct'>): BalanceResponse {
  return {
    account_id: toTypeId('acct', accountId),
    pending_in: balance.pendingIn.toString(),
    available: balance.available.toString(),
    held: balance.held.toString(),
    currency: balance.currency,
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
// opposite of the provider webhook (notificationBodySchema, webhooks/schemas.ts). This is our
// own client's request to move money, so a misspelt optional field (remitance_info) must fail
// rather than be silently dropped (slice 2 decision 33).
export const withdrawalBodySchema = z.strictObject({
  // At most the SEPA Credit Transfer maximum (decision 60).
  amount_minor: sepaAmountSchema,
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

// GET /accounts/:account_id/withdrawals/:withdrawal_id: both IDs, in the contract's order.
export const withdrawalParamsSchema = z.object({
  account_id: typeIdSchema('acct'),
  withdrawal_id: typeIdSchema('wd'),
});

export const withdrawalQuerySchema = z.object({ include: z.literal('history').optional() });

// The withdrawals list's query, in the contract's parameter order (decision 68), as the
// deposits list's. status: any withdrawal status (every withdrawal has an account).
export const withdrawalsQuerySchema = z.object({
  limit: z.coerce.number().int().min(1).max(200).default(50),
  before: typeIdSchema('wd').optional(),
  include: z.literal('history').optional(),
  status: z.enum(WITHDRAWAL_STATUSES).optional(),
});

// A withdrawal's history row as a client sees it. Unlike a deposit's, its reason is shown: the
// only one recorded is on the change to failed, and it is the provider's (its transfer update's
// reason, or its refusal's), or a generic text of ours when it gave none; meant for the customer
// (slice 4 decision 76).
export type WithdrawalStatusChangeResponse = {
  readonly from_status: WithdrawalStatus | null;
  readonly to_status: WithdrawalStatus;
  readonly reason: string | null;
  readonly at: string;
};

// type first (decision 67). No provider_transfer_id: provider identifiers stay at the edge
// (decision 69). history only with include=history.
export type WithdrawalResponse = {
  readonly type: 'withdrawal';
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

// With history when given (an array, empty if the withdrawal has none).
export function toWithdrawalResponse(
  withdrawal: Withdrawal,
  history?: readonly WithdrawalStatusChange[],
): WithdrawalResponse {
  return {
    type: 'withdrawal',
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
    ...(history === undefined ? {} : { history: history.map(toWithdrawalStatusChangeResponse) }),
  };
}

export type WithdrawalListResponse = {
  readonly data: readonly WithdrawalResponse[];
  readonly next_before: TypeIdString<'wd'> | null;
};

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
