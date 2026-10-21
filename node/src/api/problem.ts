import { STATUS_CODES } from 'node:http';
import type { FastifyReply } from 'fastify';

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

export const PROBLEM_TYPES = {
  invalidNotification: 'urn:ledger-demo:problem:invalid-notification',
  notificationConflict: 'urn:ledger-demo:problem:notification-conflict',
  unknownAccount: 'urn:ledger-demo:problem:unknown-account',
  notAnnounced: 'urn:ledger-demo:problem:payment-not-announced',
} as const;

export function httpProblem(status: number, detail?: string): Problem {
  return {
    type: 'about:blank',
    title: STATUS_CODES[status] ?? 'Error',
    status,
    ...(detail === undefined ? {} : { detail }),
  };
}

export function sendProblem(reply: FastifyReply, problem: Problem): FastifyReply {
  return reply.code(problem.status).type('application/problem+json').send(problem);
}
