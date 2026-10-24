import type pg from 'pg';
import { problemErrorHandler } from '../errors.ts';
import type { App } from '../types.ts';
import { registerIncomingPaymentRoutes } from './incoming-payments.ts';
import { registerTransferUpdateRoutes } from './transfer-updates.ts';

// The provider webhooks, as one unit: their routes (at /provider/…), their body schemas
// (./schemas.ts) and their error handling. The seam where another provider's adapter would plug
// in (README, Known gaps).
//
// A plugin with its own Fastify context: a plain async function passed to register() gets a
// child context (encapsulation), so the error handler set inside it applies only to the routes
// registered inside it. Wrapping it in fastify-plugin would remove exactly that. No prefix: the
// routes keep their full paths.
export function registerWebhookRoutes(app: App, deps: { pool: pg.Pool }): void {
  // The annotation `webhooks: App` (no cast): at run time the child context uses the root's
  // logger instance, our pino Logger, but Fastify's types don't carry the parent's logger type
  // into register() (types/instance.d.ts passes register() only the instance type; Fastify
  // 5.12.7), so the child would be typed with FastifyBaseLogger. The annotation states the real
  // type, and TypeScript checks it against register()'s constraint. Delete it once Fastify's
  // types carry the logger through.
  void app.register(async (webhooks: App) => {
    // The webhook contract (slice 2 decision 37): a provider retries anything but a 2xx, so any
    // problem with the request (a malformed, oversized or non-JSON body: Fastify's 400, 413, 415)
    // is answered 400, the contract's one 4xx: a body that can't be used. Unexpected errors are
    // still 500.
    webhooks.setErrorHandler(problemErrorHandler(() => 400));
    registerIncomingPaymentRoutes(webhooks, deps);
    registerTransferUpdateRoutes(webhooks, deps);
  });
}
