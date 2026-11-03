import { getAccount } from '../accounts/accounts.ts';
import { listDeposits } from '../deposits/deposits.ts';
import { getBalance } from '../ledger/balances.ts';
import type { Queryable } from '../shared/db.ts';
import { toTypeId } from '../shared/ids.ts';
import { statusHistories } from '../deposits/status.ts';
import { problems, requestErrors, sendProblem, validationErrors } from './problem.ts';
import {
  accountParamsSchema,
  toAccountResponse,
  toBalanceResponse,
  toDepositResponse,
  toStatusChangeResponse,
  transactionsQuerySchema,
  type TransactionListResponse,
} from './schemas.ts';
import type { App } from './types.ts';

// Read-only account endpoints. No authentication yet (see Known gaps): anyone who can reach
// the API can read any account.
export function registerAccountRoutes(app: App, deps: { db: Queryable }): void {
  // The account itself (slice 4 decision 71).
  app.get('/accounts/:account_id', async (request, reply) => {
    const params = accountParamsSchema.safeParse(request.params);
    if (!params.success) {
      return sendProblem(reply, problems.invalidRequest(validationErrors(params.error)));
    }

    const account = await getAccount(deps.db, params.data.account_id);
    if (account === undefined) return sendProblem(reply, problems.accountNotFound());
    return reply.send(toAccountResponse(account));
  });

  app.get('/accounts/:account_id/balance', async (request, reply) => {
    const params = accountParamsSchema.safeParse(request.params);
    if (!params.success) {
      return sendProblem(reply, problems.invalidRequest(validationErrors(params.error)));
    }

    const balance = await getBalance(deps.db, params.data.account_id);
    if (balance === undefined) return sendProblem(reply, problems.accountNotFound());
    return reply.send(toBalanceResponse(balance, params.data.account_id));
  });

  // Deposits newest first, a page at a time: ?limit (default 50, at most 200) and
  // ?before=<next_before from the previous page>. ?include=history adds each deposit's
  // status history, fetched for the whole page in one query.
  app.get('/accounts/:account_id/transactions', async (request, reply) => {
    // Every problem at once, path then query (slice 4 decision 68).
    const params = accountParamsSchema.safeParse(request.params);
    const query = transactionsQuerySchema.safeParse(request.query);
    if (!params.success || !query.success) {
      return sendProblem(reply, problems.invalidRequest(requestErrors(params, query)));
    }

    const page = await listDeposits(deps.db, params.data.account_id, {
      limit: query.data.limit,
      ...(query.data.before === undefined ? {} : { before: query.data.before }),
    });
    if (page === undefined) return sendProblem(reply, problems.accountNotFound());

    const histories =
      query.data.include === 'history'
        ? await statusHistories(
            deps.db,
            page.deposits.map((p) => p.id),
          )
        : undefined;

    const response: TransactionListResponse = {
      deposits: page.deposits.map((deposit) => ({
        ...toDepositResponse(deposit),
        ...(histories === undefined
          ? {}
          : { history: (histories.get(deposit.id) ?? []).map(toStatusChangeResponse) }),
      })),
      next_before: page.nextBefore === null ? null : toTypeId('dep', page.nextBefore),
    };
    return reply.send(response);
  });
}
