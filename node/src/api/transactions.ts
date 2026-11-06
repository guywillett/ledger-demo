import type { Queryable } from '../shared/db.ts';
import { toTypeId, type TypeIdString } from '../shared/ids.ts';
import { listTransactions, type TransactionRef } from '../transactions/transactions.ts';
import { problems, requestErrors, sendProblem } from './problem.ts';
import {
  accountParamsSchema,
  toDepositResponse,
  toWithdrawalResponse,
  transactionsQuerySchema,
  type TransactionListResponse,
} from './schemas.ts';
import type { App } from './types.ts';

// GET /accounts/:account_id/transactions: the account's deposits and withdrawals together, newest
// first (slice 4 decision 67; how the page is read: transactions/transactions.ts). Each item is
// exactly what its own endpoint returns, through the same response mapping, without history.
export function registerTransactionRoutes(app: App, deps: { db: Queryable }): void {
  app.get('/accounts/:account_id/transactions', async (request, reply) => {
    // Every problem at once, path then query (decision 68).
    const params = accountParamsSchema.safeParse(request.params);
    const query = transactionsQuerySchema.safeParse(request.query);
    if (!params.success || !query.success) {
      return sendProblem(reply, problems.invalidRequest(requestErrors(params, query)));
    }

    const page = await listTransactions(deps.db, params.data.account_id, {
      limit: query.data.limit,
      ...(query.data.before === undefined ? {} : { before: query.data.before }),
    });
    // A list checks its account: an unknown one is a 404, an empty list an account without any.
    if (page === undefined) return sendProblem(reply, problems.accountNotFound());

    const response: TransactionListResponse = {
      data: page.transactions.map((t) =>
        t.type === 'deposit' ? toDepositResponse(t.deposit) : toWithdrawalResponse(t.withdrawal),
      ),
      next_before: page.nextBefore === null ? null : cursorOf(page.nextBefore),
    };
    return reply.send(response);
  });
}

// The item's TypeID, the next page's cursor.
function cursorOf(ref: TransactionRef): TypeIdString<'dep'> | TypeIdString<'wd'> {
  return ref.type === 'deposit' ? toTypeId('dep', ref.id) : toTypeId('wd', ref.id);
}
