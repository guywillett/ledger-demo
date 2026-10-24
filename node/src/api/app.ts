import { randomUUID } from 'node:crypto';
import Fastify, { LogController, type FastifyReply, type FastifyRequest } from 'fastify';
import type pg from 'pg';
import type { Logger } from '../shared/logger.ts';
import { registerAccountRoutes } from './accounts.ts';
import { problemErrorHandler } from './errors.ts';
import { problems, sendProblem } from './problem.ts';
import { registerWebhookRoutes } from './webhooks/index.ts';
import { registerWithdrawalRoutes } from './withdrawals.ts';
import type { App } from './types.ts';

// An incoming x-request-id is reused only if it is short and plain: it ends up in every log
// line for the request. Anything else is ignored (not rejected) and a new ID generated.
const REQUEST_ID_PATTERN = /^[A-Za-z0-9._:-]{1,64}$/;

export function requestIdFrom(header: string | string[] | undefined): string {
  return typeof header === 'string' && REQUEST_ID_PATTERN.test(header) ? header : randomUUID();
}

// Fastify's LogController writes its per-request log lines. By default that is two lines,
// "incoming request" and "request completed"; we want one access line with flat fields.
class AccessLog extends LogController {
  override incomingRequest(): void {}

  override requestCompleted(
    error: Error | null | undefined,
    request: FastifyRequest,
    reply: FastifyReply,
  ): void {
    const fields = {
      method: request.method,
      // The path only: a query string is caller input and could carry anything (an IBAN, a name).
      path: request.url.split('?', 1)[0] ?? '',
      status_code: reply.statusCode,
      response_time_ms: Math.round(reply.elapsedTime),
    };
    if (error) {
      reply.log.error({ ...fields, err: error }, 'request errored');
    } else {
      reply.log.info(fields, 'request completed');
    }
  }
}

// Builds the app without listening, so tests can drive it with app.inject().
export function buildApp(deps: { pool: pg.Pool; logger: Logger }): App {
  const app = Fastify({
    loggerInstance: deps.logger,
    logController: new AccessLog({ requestIdLogLabel: 'request_id' }),
    genReqId: (req) => requestIdFrom(req.headers['x-request-id']),
  });

  app.addHook('onSend', async (request, reply) => {
    reply.header('x-request-id', request.id);
  });

  // The client API answers a client error with its exact code (problem details); the provider
  // webhooks set their own handler in their plugin (api/webhooks/index.ts).
  app.setErrorHandler(problemErrorHandler((status) => status));

  // A fixed detail: echoing the requested URL would return caller input.
  app.setNotFoundHandler((_request, reply) => {
    return sendProblem(reply, problems.http(404, 'No such route.'));
  });

  // Fastify parses text/plain bodies by default, which would let a text/plain body reach
  // validation (400). We accept JSON only, so every other content type gets 415 (400 on the
  // provider webhooks, whose plugin inherits this and answers every client error with 400).
  app.removeContentTypeParser('text/plain');

  registerAccountRoutes(app, { db: deps.pool });
  registerWithdrawalRoutes(app, { pool: deps.pool });
  registerWebhookRoutes(app, { pool: deps.pool });
  return app;
}
