import { STATUS_CODES } from 'node:http';
import type { FastifyReply } from 'fastify';
import type { z } from 'zod';
import type { ComparedField } from '../ingestion/notifications.ts';
import type { ComparedWithdrawalField } from '../withdrawals/requests.ts';

// RFC 9457 problem details. `type` identifies the kind of problem: a URN for our own types
// (an identifier, not a URL that pretends to resolve), or "about:blank" for plain HTTP errors,
// where the title is the HTTP status phrase.
export type Problem = {
  readonly type: string;
  readonly title: string;
  readonly status: number;
  readonly detail?: string;
  readonly [extension: string]: unknown;
};

// One entry of a 400's `errors`.
export type ValidationError = { readonly path: string; readonly message: string };

// Every problem this API answers with, each defined once: its type, title and status live
// here, and call sites pass only what varies (the validation errors, the differing fields).
// The contract (contracts/openapi.yaml) documents the same set.
//
// `satisfies` checks that every entry builds a valid Problem without widening the entries to
// that type, so each constructor keeps its exact shape (e.g. differing_fields stays typed).
export const problems = {
  invalidNotification: (errors: readonly ValidationError[]) => ({
    type: 'urn:ledger-demo:problem:invalid-notification',
    title: 'Invalid payment notification',
    status: 400,
    errors,
  }),
  notificationConflict: (differingFields: readonly ComparedField[]) => ({
    type: 'urn:ledger-demo:problem:notification-conflict',
    title: 'Notification conflicts with a stored payment',
    status: 409,
    detail:
      'A payment with this network transaction ID exists with different details. ' +
      'This is not treated as a retry.',
    differing_fields: differingFields,
  }),
  unknownAccount: () => ({
    type: 'urn:ledger-demo:problem:unknown-account',
    title: 'Unknown account',
    status: 422,
    detail: 'The creditor IBAN does not match any account managed by this application.',
  }),
  invalidRequest: (errors: readonly ValidationError[]) => ({
    type: 'urn:ledger-demo:problem:invalid-request',
    title: 'Invalid request',
    status: 400,
    errors,
  }),
  accountNotFound: () => ({
    type: 'urn:ledger-demo:problem:account-not-found',
    title: 'Account not found',
    status: 404,
  }),
  withdrawalNotFound: () => ({
    type: 'urn:ledger-demo:problem:withdrawal-not-found',
    title: 'Withdrawal not found',
    status: 404,
  }),
  insufficientFunds: () => ({
    type: 'urn:ledger-demo:problem:insufficient-funds',
    title: 'Insufficient funds',
    status: 422,
    detail:
      'The available balance does not cover the withdrawal. Nothing was stored, so the same ' +
      'Idempotency-Key can be used again.',
  }),
  idempotencyKeyConflict: (differingFields: readonly ComparedWithdrawalField[]) => ({
    type: 'urn:ledger-demo:problem:idempotency-key-conflict',
    title: 'Idempotency key reused with different details',
    status: 409,
    detail:
      'This account already has a withdrawal with this Idempotency-Key and different details. ' +
      'Use a new key for a new withdrawal.',
    differing_fields: differingFields,
  }),
  // A plain HTTP error (Fastify's own 4xx, unknown routes, unexpected 500s).
  http: (status: number, detail?: string) => ({
    type: 'about:blank',
    title: STATUS_CODES[status] ?? 'Error',
    status,
    ...(detail === undefined ? {} : { detail }),
  }),
} satisfies Record<string, (...args: never[]) => Problem>;

// A 400's `errors`: the path and message of each Zod issue, never the input values (they come
// from the request).
export function validationErrors(error: z.ZodError): ValidationError[] {
  return error.issues.map((issue) => ({
    path: issue.path.map(String).join('.'),
    message: issue.message,
  }));
}

export function sendProblem(reply: FastifyReply, problem: Problem): FastifyReply {
  return reply.code(problem.status).type('application/problem+json').send(problem);
}
