import type { FastifyReply, FastifyRequest } from 'fastify';
import { problems, sendProblem } from './problem.ts';

// Fastify's own errors (malformed JSON, unsupported content type, body too large) carry a
// 4xx statusCode. Anything else thrown is unexpected: 500. Fastify 5 types the error as
// unknown (anything can be thrown), so check its shape rather than assume it.
function clientErrorStatus(error: unknown): number | undefined {
  if (typeof error !== 'object' || error === null || !('statusCode' in error)) return undefined;
  const status = error.statusCode;
  return typeof status === 'number' && status >= 400 && status < 500 ? status : undefined;
}

// The error handler every part of the API uses, answering with problem details: an unexpected
// error is a 500 (logged at error level); a client error is logged at warn and answered with the
// status mapClientStatus picks for it. The client API keeps the exact code; the provider webhooks
// answer every client error with 400 (api/webhooks/index.ts).
export function problemErrorHandler(
  mapClientStatus: (status: number) => number,
): (error: unknown, request: FastifyRequest, reply: FastifyReply) => FastifyReply {
  return (error, request, reply) => {
    const status = clientErrorStatus(error);
    if (status === undefined) {
      request.log.error({ err: error }, 'unexpected error');
      return sendProblem(reply, problems.http(500));
    }
    const message = error instanceof Error ? error.message : undefined;
    const answered = mapClientStatus(status);
    request.log.warn({ status_code: answered, reason: message }, 'request rejected');
    return sendProblem(reply, problems.http(answered, message));
  };
}
