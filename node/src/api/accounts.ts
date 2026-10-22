import { listPayments } from '../payments/payments.ts';
import { getBalance } from '../ledger/balances.ts';
import type { Queryable } from '../shared/db.ts';
import { toTypeId } from '../shared/ids.ts';
import { statusHistories } from '../payments/status.ts';
import { problems, sendProblem, validationErrors } from './problem.ts';
import {
  accountParamsSchema,
  toBalanceResponse,
  toPaymentResponse,
  toStatusChangeResponse,
  transactionsQuerySchema,
  type TransactionListResponse,
} from './schemas.ts';
import type { App } from './types.ts';

// Read-only account endpoints. No authentication yet (see Known gaps): anyone who can reach
// the API can read any account.
export function registerAccountRoutes(app: App, deps: { db: Queryable }): void {
  app.get('/accounts/:account_id/balance', async (request, reply) => {
    const params = accountParamsSchema.safeParse(request.params);
    if (!params.success) {
      return sendProblem(reply, problems.invalidRequest(validationErrors(params.error)));
    }

    const balance = await getBalance(deps.db, params.data.account_id);
    if (balance === undefined) return sendProblem(reply, problems.accountNotFound());
    return reply.send(toBalanceResponse(balance));
  });

  // Payments newest first, a page at a time: ?limit (default 50, at most 200) and
  // ?before=<next_before from the previous page>. ?include=history adds each payment's
  // status history, fetched for the whole page in one query.
  app.get('/accounts/:account_id/transactions', async (request, reply) => {
    const params = accountParamsSchema.safeParse(request.params);
    if (!params.success) {
      return sendProblem(reply, problems.invalidRequest(validationErrors(params.error)));
    }
    const query = transactionsQuerySchema.safeParse(request.query);
    if (!query.success) {
      return sendProblem(reply, problems.invalidRequest(validationErrors(query.error)));
    }

    const page = await listPayments(deps.db, params.data.account_id, {
      limit: query.data.limit,
      ...(query.data.before === undefined ? {} : { before: query.data.before }),
    });
    if (page === undefined) return sendProblem(reply, problems.accountNotFound());

    const histories =
      query.data.include === 'history'
        ? await statusHistories(
            deps.db,
            page.payments.map((p) => p.id),
          )
        : undefined;

    const response: TransactionListResponse = {
      payments: page.payments.map((payment) => ({
        ...toPaymentResponse(payment),
        ...(histories === undefined
          ? {}
          : { history: (histories.get(payment.id) ?? []).map(toStatusChangeResponse) }),
      })),
      next_before: page.nextBefore === null ? null : toTypeId('pay', page.nextBefore),
    };
    return reply.send(response);
  });
}
