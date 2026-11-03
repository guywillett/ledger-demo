import { getAccount } from '../accounts/accounts.ts';
import { getBalance } from '../ledger/balances.ts';
import type { Queryable } from '../shared/db.ts';
import { problems, sendProblem, validationErrors } from './problem.ts';
import { accountParamsSchema, toAccountResponse, toBalanceResponse } from './schemas.ts';
import type { App } from './types.ts';

// Read-only account endpoints: the account and its balance (its deposits are in deposits.ts). No
// authentication yet (see Known gaps): anyone who can reach the API can read any account.
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
}
