import { getDeposit, listDeposits } from '../deposits/deposits.ts';
import { statusHistories } from '../deposits/status.ts';
import type { Queryable } from '../shared/db.ts';
import { toTypeId } from '../shared/ids.ts';
import { problems, requestErrors, sendProblem } from './problem.ts';
import {
  accountParamsSchema,
  depositParamsSchema,
  depositQuerySchema,
  depositsQuerySchema,
  toDepositResponse,
  type DepositListResponse,
} from './schemas.ts';
import type { App } from './types.ts';

// An account's deposits, as a list and singly, within the account (slice 4 decision 64: every
// query takes the account). No authentication yet (see Known gaps).
export function registerDepositRoutes(app: App, deps: { db: Queryable }): void {
  // Newest first, a page at a time: ?limit (default 50, at most 200) and ?before=<next_before
  // from the previous page>; {data, next_before} (decision 66). ?include=history adds each
  // deposit's status history, read for the whole page in one query. ?status and
  // ?network_transaction_id filter it (decision 70), the page and its cursor within the filtered
  // list.
  app.get('/accounts/:account_id/deposits', async (request, reply) => {
    // Every problem at once, path then query (slice 4 decision 68).
    const params = accountParamsSchema.safeParse(request.params);
    const query = depositsQuerySchema.safeParse(request.query);
    if (!params.success || !query.success) {
      return sendProblem(reply, problems.invalidRequest(requestErrors(params, query)));
    }

    const page = await listDeposits(deps.db, params.data.account_id, {
      limit: query.data.limit,
      ...(query.data.before === undefined ? {} : { before: query.data.before }),
      ...(query.data.status === undefined ? {} : { status: query.data.status }),
      ...(query.data.network_transaction_id === undefined
        ? {}
        : { networkTransactionId: query.data.network_transaction_id }),
    });
    // A list checks its account: an unknown one is a 404, an empty list an account without any.
    if (page === undefined) return sendProblem(reply, problems.accountNotFound());

    const histories =
      query.data.include === 'history'
        ? await statusHistories(
            deps.db,
            page.deposits.map((p) => p.id),
          )
        : undefined;

    const response: DepositListResponse = {
      data: page.deposits.map((deposit) =>
        toDepositResponse(
          deposit,
          histories === undefined ? undefined : (histories.get(deposit.id) ?? []),
        ),
      ),
      next_before: page.nextBefore === null ? null : toTypeId('dep', page.nextBefore),
    };
    return reply.send(response);
  });

  // One deposit, read with its account in one query: another account's deposit is a 404, the
  // same as one that doesn't exist, whether or not the account exists (decision 64).
  // ?include=history adds its status history.
  app.get('/accounts/:account_id/deposits/:deposit_id', async (request, reply) => {
    const params = depositParamsSchema.safeParse(request.params);
    const query = depositQuerySchema.safeParse(request.query);
    if (!params.success || !query.success) {
      return sendProblem(reply, problems.invalidRequest(requestErrors(params, query)));
    }

    const deposit = await getDeposit(deps.db, params.data.account_id, params.data.deposit_id);
    if (deposit === undefined) return sendProblem(reply, problems.depositNotFound());

    if (query.data.include !== 'history') return reply.send(toDepositResponse(deposit));
    const histories = await statusHistories(deps.db, [deposit.id]);
    return reply.send(toDepositResponse(deposit, histories.get(deposit.id) ?? []));
  });
}
