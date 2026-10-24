import { z } from 'zod';
import { toTypeId } from '../shared/ids.ts';
import type { Withdrawal } from '../withdrawals/withdrawals.ts';

// The provider's HTTP API, from the submitter's side: one call, POST /transfers, mapped to an
// outcome. The boundary to the outside world, so the handler stays pure logic and its tests use
// a fake client.

export type SubmitOutcome =
  // 201, or 200 for a repeat of our key: the provider has the transfer.
  | { readonly kind: 'accepted'; readonly transferId: string }
  // 422: a permanent refusal; nothing was created at the provider.
  | { readonly kind: 'refused'; readonly reason: string }
  // 400: our request is wrong (a bug on our side). Only a summary of the provider's body, made
  // here at the boundary: a real provider may echo request values (an IBAN, a name) back in its
  // errors, and those must not reach the logs. `fields`: the field paths its errors name, kept
  // only if they are plain identifiers; `bodyBytes`: the body's size.
  | { readonly kind: 'invalid'; readonly fields: readonly string[]; readonly bodyBytes: number }
  // 409: our key with another body. We always resend the identical request, so this is a bug.
  // That meaning is provider-sim's contract. Real providers differ: some also answer 409 while a
  // request with the same key is still in progress, which a retry right after a timeout can hit,
  // and which is an unknown outcome, not a bug. A real integration maps its provider's answers
  // to these kinds (typically by the error code in the body); see Known gaps in the slice 2 spec.
  | { readonly kind: 'key_conflict' }
  // Anything else: a timeout, a network error, a 5xx, an answer we can't read.
  //
  // The important kind. The provider may or may not have created the transfer: a timeout after
  // the request was sent is the classic case (it may have arrived and been processed, with only
  // the answer lost). So the outcome is never guessed (slice 2 decision 26): the call is made
  // again later with the same Idempotency-Key, and the provider's idempotency turns the retry
  // into "tell me what happened to this one". Never a new key for the same withdrawal.
  | { readonly kind: 'unknown'; readonly cause: string };

export type ProviderClient = {
  submitTransfer(w: Withdrawal): Promise<SubmitOutcome>;
};

// Response bodies are validated like any other input (Zod at every boundary). Unknown fields
// are ignored: providers add fields over time.
const acceptedSchema = z.object({ transfer_id: z.string().min(1).max(255) });
const refusedSchema = z.object({ reason: z.string().max(1000) });
// From a 400: the field paths only (each error's message may repeat the input). A path is kept
// only if it looks like a field name, so nothing else can slip through.
const FIELD_PATH = /^[A-Za-z0-9_.-]{1,64}$/;
const invalidSchema = z.object({ errors: z.array(z.object({ path: z.string() })).max(50) });

function errorFields(payload: unknown): string[] {
  const parsed = invalidSchema.safeParse(payload);
  if (!parsed.success) return [];
  return parsed.data.errors.map((e) => e.path).filter((path) => FIELD_PATH.test(path));
}

export function providerClient(options: { baseUrl: string; timeoutMs: number }): ProviderClient {
  const url = new URL('/transfers', options.baseUrl);
  return {
    async submitTransfer(w) {
      const reference = toTypeId('wd', w.id);
      // Built only from the stored withdrawal, so every attempt sends the identical request.
      const body = {
        amount_minor: w.amountMinor.toString(),
        currency: w.currency,
        creditor_iban: w.creditorIban,
        creditor_name: w.creditorName,
        remittance_info: w.remittanceInfo,
        reference,
      };

      let response: Response;
      let text: string;
      let payload: unknown;
      try {
        // fetch() itself never times out; AbortSignal.timeout aborts a call that hangs.
        response = await fetch(url, {
          method: 'POST',
          headers: { 'content-type': 'application/json', 'idempotency-key': reference },
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(options.timeoutMs),
        });
        text = await response.text();
        payload = parseJson(text);
      } catch (err) {
        // Timeout, connection refused, reset mid-response: the request may have arrived.
        return { kind: 'unknown', cause: err instanceof Error ? err.message : String(err) };
      }

      switch (response.status) {
        case 200:
        case 201: {
          const accepted = acceptedSchema.safeParse(payload);
          // A 2xx we can't read is not an acceptance: the next attempt with the same key gets
          // the transfer ID.
          return accepted.success
            ? { kind: 'accepted', transferId: accepted.data.transfer_id }
            : { kind: 'unknown', cause: `unreadable ${response.status} response` };
        }
        case 422: {
          const refused = refusedSchema.safeParse(payload);
          return refused.success
            ? { kind: 'refused', reason: refused.data.reason || 'refused by the provider' }
            : { kind: 'unknown', cause: 'unreadable 422 response' };
        }
        case 400:
          return {
            kind: 'invalid',
            fields: errorFields(payload),
            bodyBytes: Buffer.byteLength(text),
          };
        case 409:
          return { kind: 'key_conflict' };
        default:
          return { kind: 'unknown', cause: `provider answered ${response.status}` };
      }
    },
  };
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}
