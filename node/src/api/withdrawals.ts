import type pg from 'pg';
import { InsufficientFundsError } from '../ledger/withdrawals.ts';
import { withTransaction } from '../shared/db.ts';
import { maskIban } from '../shared/iban.ts';
import { toTypeId } from '../shared/ids.ts';
import { requestWithdrawal, type RequestResult } from '../withdrawals/requests.ts';
import { withdrawalStatusHistories, withdrawalStatusHistory } from '../withdrawals/status.ts';
import {
  AccountNotFoundError,
  getWithdrawalInAccount,
  listWithdrawals,
} from '../withdrawals/withdrawals.ts';
import { problems, requestErrors, sendProblem } from './problem.ts';
import {
  accountParamsSchema,
  idempotencyKeyHeadersSchema,
  toNewWithdrawal,
  toWithdrawalResponse,
  withdrawalBodySchema,
  withdrawalParamsSchema,
  withdrawalQuerySchema,
  withdrawalsQuerySchema,
  type WithdrawalListResponse,
} from './schemas.ts';
import type { App } from './types.ts';

// Client withdrawals. POST holds the funds in the request's own transaction (slice 2 decision
// 25), together with the withdrawal.held event in the outbox, so the client learns at once
// whether its withdrawal is accepted (201) or refused for insufficient funds (422). The relay
// publishes the event; nothing here talks to NATS.
//
// Logs never contain the creditor name or remittance text, and the IBAN only masked.
export function registerWithdrawalRoutes(app: App, deps: { pool: pg.Pool }): void {
  app.post('/accounts/:account_id/withdrawals', async (request, reply) => {
    // Every problem with the request at once: path, header and body (slice 4 decision 68).
    const params = accountParamsSchema.safeParse(request.params);
    const headers = idempotencyKeyHeadersSchema.safeParse(request.headers);
    const body = withdrawalBodySchema.safeParse(request.body);
    if (!params.success || !headers.success || !body.success) {
      const errors = requestErrors(params, headers, body);
      request.log.warn({ errors }, 'invalid withdrawal request');
      return sendProblem(reply, problems.invalidRequest(errors));
    }

    const withdrawalRequest = toNewWithdrawal(
      params.data.account_id,
      headers.data['idempotency-key'],
      body.data,
    );
    const context = {
      account_id: toTypeId('acct', withdrawalRequest.accountId),
      idempotency_key: withdrawalRequest.idempotencyKey,
    };

    // Insufficient funds and an unknown account are thrown inside the transaction and caught
    // here, outside it: by then it has rolled back, nothing is stored and the key is still free.
    let result: RequestResult;
    try {
      result = await withTransaction(deps.pool, (tx) => requestWithdrawal(tx, withdrawalRequest));
    } catch (err) {
      if (err instanceof InsufficientFundsError) {
        // A business outcome, not a problem with our system or a client bug.
        request.log.info(
          { ...context, amount_minor: withdrawalRequest.amountMinor },
          'withdrawal refused: insufficient funds',
        );
        return sendProblem(reply, problems.insufficientFunds());
      }
      if (err instanceof AccountNotFoundError) {
        request.log.warn(context, 'withdrawal request for an unknown account');
        return sendProblem(reply, problems.accountNotFound());
      }
      throw err;
    }

    switch (result.kind) {
      case 'created': {
        const id = toTypeId('wd', result.withdrawal.id);
        request.log.info(
          {
            ...context,
            withdrawal_id: id,
            amount_minor: result.withdrawal.amountMinor,
            creditor_iban: maskIban(result.withdrawal.creditorIban),
            event_id: toTypeId('evt', result.eventId),
          },
          'withdrawal created',
        );
        return reply
          .code(201)
          .header('location', `/accounts/${context.account_id}/withdrawals/${id}`)
          .send(toWithdrawalResponse(result.withdrawal));
      }
      case 'repeat': {
        request.log.info(
          {
            ...context,
            withdrawal_id: toTypeId('wd', result.withdrawal.id),
            status: result.withdrawal.status,
          },
          'repeated withdrawal request',
        );
        return reply.code(200).send(toWithdrawalResponse(result.withdrawal));
      }
      case 'conflict': {
        // The client reused a key for another withdrawal: its bug, not ours.
        request.log.warn(
          {
            ...context,
            withdrawal_id: toTypeId('wd', result.stored.id),
            differing_fields: result.differingFields,
          },
          'idempotency key reused with different details',
        );
        return sendProblem(reply, problems.idempotencyKeyConflict(result.differingFields));
      }
    }
  });

  // The account's withdrawals, newest first, a page at a time: ?limit, ?before=<next_before>,
  // ?include=history and the ?status filter, as the deposits list (decisions 66 and 70).
  app.get('/accounts/:account_id/withdrawals', async (request, reply) => {
    // Every problem at once, path then query (decision 68).
    const params = accountParamsSchema.safeParse(request.params);
    const query = withdrawalsQuerySchema.safeParse(request.query);
    if (!params.success || !query.success) {
      return sendProblem(reply, problems.invalidRequest(requestErrors(params, query)));
    }

    const page = await listWithdrawals(deps.pool, params.data.account_id, {
      limit: query.data.limit,
      ...(query.data.before === undefined ? {} : { before: query.data.before }),
      ...(query.data.status === undefined ? {} : { status: query.data.status }),
    });
    // A list checks its account: an unknown one is a 404, an empty list an account without any.
    if (page === undefined) return sendProblem(reply, problems.accountNotFound());

    const histories =
      query.data.include === 'history'
        ? await withdrawalStatusHistories(
            deps.pool,
            page.withdrawals.map((w) => w.id),
          )
        : undefined;
    const response: WithdrawalListResponse = {
      data: page.withdrawals.map((withdrawal) =>
        toWithdrawalResponse(
          withdrawal,
          histories === undefined ? undefined : (histories.get(withdrawal.id) ?? []),
        ),
      ),
      next_before: page.nextBefore === null ? null : toTypeId('wd', page.nextBefore),
    };
    return reply.send(response);
  });

  // One withdrawal in its current status, read with its account in one query: another
  // account's withdrawal is a 404, the same as one that doesn't exist, whether or not the
  // account exists (decision 64). ?include=history adds its status history.
  app.get('/accounts/:account_id/withdrawals/:withdrawal_id', async (request, reply) => {
    const params = withdrawalParamsSchema.safeParse(request.params);
    const query = withdrawalQuerySchema.safeParse(request.query);
    if (!params.success || !query.success) {
      return sendProblem(reply, problems.invalidRequest(requestErrors(params, query)));
    }

    const withdrawal = await getWithdrawalInAccount(
      deps.pool,
      params.data.account_id,
      params.data.withdrawal_id,
    );
    if (withdrawal === undefined) return sendProblem(reply, problems.withdrawalNotFound());

    if (query.data.include !== 'history') return reply.send(toWithdrawalResponse(withdrawal));
    const history = await withdrawalStatusHistory(deps.pool, withdrawal.id);
    return reply.send(toWithdrawalResponse(withdrawal, history));
  });
}
