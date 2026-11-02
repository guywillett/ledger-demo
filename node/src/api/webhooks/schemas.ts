import { z } from 'zod';
import type { Notification } from '../../ingestion/incoming-payments.ts';
import type { TransferUpdate } from '../../ingestion/transfer-updates.ts';
import { fromTypeId, type Uuid } from '../../shared/ids.ts';
import { ibanSchema, sepaAmountSchema } from '../../shared/schemas.ts';

// The provider webhooks' request bodies and their mapping to ingestion's types. The client
// API's request schemas, and every response type, are in ../schemas.ts.

// POST /provider/incoming-payments body. z.object (not strictObject) drops unknown fields
// instead of rejecting them: providers add fields to webhooks over time ("tolerant reader").
// Dropped fields are neither stored nor logged. Client requests are the opposite
// (withdrawalBodySchema in ../schemas.ts, slice 2 decision 33).
export const notificationBodySchema = z.object({
  network_transaction_id: z.string().min(1).max(255),
  creditor_iban: ibanSchema,
  provider_account_id: z.string().min(1).max(255).optional(),
  // At most the SEPA Credit Transfer maximum (decision 60).
  amount_minor: sepaAmountSchema,
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

// POST /provider/transfer-updates body: the provider's result for one transfer. A tolerant
// reader (z.object), like POST /provider/incoming-payments: providers add fields to webhooks
// over time.
export const transferUpdateBodySchema = z
  .object({
    provider_event_id: z.string().min(1).max(255),
    transfer_id: z.string().min(1).max(255),
    // Our withdrawal TypeID, as we sent it; anything else is kept as text and unmatched.
    reference: z.string().min(1).max(255),
    status: z.enum(['executed', 'failed']),
    reason: z.string().min(1).max(1000).optional(),
    occurred_at: z.iso.datetime(),
  })
  .refine((u) => (u.status === 'failed') === (u.reason !== undefined), {
    error: 'is required for failed, and must be absent for executed',
    path: ['reason'],
  });

export function toTransferUpdate(body: z.infer<typeof transferUpdateBodySchema>): TransferUpdate {
  return {
    providerEventId: body.provider_event_id,
    transferId: body.transfer_id,
    reference: body.reference,
    withdrawalId: withdrawalIdOrNull(body.reference),
    status: body.status,
    reason: body.reason ?? null,
    occurredAt: new Date(body.occurred_at),
  };
}

// The reference as a withdrawal ID if it is a wd_ TypeID holding a UUIDv7; null otherwise (the
// update is then recorded as unmatched, not refused).
function withdrawalIdOrNull(reference: string): Uuid<'wd'> | null {
  try {
    return fromTypeId('wd', reference);
  } catch {
    return null;
  }
}
