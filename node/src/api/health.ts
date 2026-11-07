import type pg from 'pg';
import { ping } from '../shared/db.ts';
import { problems, sendProblem } from './problem.ts';
import type { App } from './types.ts';

// GET /health: readiness (slice 4 decision 72). 200 {"status":"ok"} if the database answers
// SELECT 1 within HEALTH_TIMEOUT_MS, else a 503 problem. Outside the account scope: probes
// (the scripts, a load balancer, an orchestrator) call it, not clients acting on an account.
//
// Logged only when it fails: probes call it often, and a line per probe would bury the access
// log. The route's accessLog config tells AccessLog (api/app.ts) to skip the line for a success.

// Long enough for a healthy database, a new connection included (milliseconds), short enough
// that a probe hears "not ready" before its own timeout gives up on us. Go's healthTimeout
// (go/internal/api/health.go) is the same.
export const HEALTH_TIMEOUT_MS = 1000;

// Fastify route config is typed by augmenting its FastifyContextConfig interface (TypeScript
// merges an interface declared again in a module augmentation with the original).
declare module 'fastify' {
  interface FastifyContextConfig {
    // 'failures': no access-log line for a response below 400.
    accessLog?: 'failures';
  }
}

export function registerHealthRoute(app: App, deps: { pool: pg.Pool }): void {
  app.get('/health', { config: { accessLog: 'failures' } }, async (request, reply) => {
    try {
      await ping(deps.pool, HEALTH_TIMEOUT_MS);
    } catch (err) {
      request.log.error({ err }, 'health check failed');
      return sendProblem(reply, problems.notReady());
    }
    return reply.send({ status: 'ok' });
  });
}
