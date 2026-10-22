import type pg from 'pg';
import { InsufficientFundsError } from '../ledger/withdrawals.ts';
import { withTransaction } from '../shared/db.ts';
import { maskIban } from '../shared/iban.ts';
import { toTypeId } from '../shared/ids.ts';
import { requestWithdrawal, type RequestResult } from '../withdrawals/requests.ts';
import { withdrawalStatusHistory } from '../withdrawals/status.ts';
import { AccountNotFoundError, getWithdrawal } from '../withdrawals/withdrawals.ts';
import { problems, sendProblem, validationErrors, type ValidationError } from './problem.ts';
import {
  accountParamsSchema,
  idempotencyKeyHeadersSchema,
  toNewWithdrawal,
  toWithdrawalResponse,
  toWithdrawalStatusChangeResponse,
  withdrawalBodySchema,
  withdrawalParamsSchema,
  withdrawalQuerySchema,
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
    // Every problem with the request at once: path, header and body.
    const params = accountParamsSchema.safeParse(request.params);
    const headers = idempotencyKeyHeadersSchema.safeParse(request.headers);
    const body = withdrawalBodySchema.safeParse(request.body);
    if (!params.success || !headers.success || !body.success) {
      const errors: ValidationError[] = [
        ...(params.success ? [] : validationErrors(params.error)),
        ...(headers.success ? [] : validationErrors(headers.error)),
        ...(body.success ? [] : validationErrors(body.error)),
      ];
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
          .header('location', `/withdrawals/${id}`)
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

  // The withdrawal in its current status; ?include=history adds its status history.
  app.get('/withdrawals/:withdrawal_id', async (request, reply) => {
    const params = withdrawalParamsSchema.safeParse(request.params);
    if (!params.success) {
      return sendProblem(reply, problems.invalidRequest(validationErrors(params.error)));
    }
    const query = withdrawalQuerySchema.safeParse(request.query);
    if (!query.success) {
      return sendProblem(reply, problems.invalidRequest(validationErrors(query.error)));
    }

    const withdrawal = await getWithdrawal(deps.pool, params.data.withdrawal_id);
    if (withdrawal === undefined) return sendProblem(reply, problems.withdrawalNotFound());

    if (query.data.include !== 'history') return reply.send(toWithdrawalResponse(withdrawal));
    const history = await withdrawalStatusHistory(deps.pool, withdrawal.id);
    return reply.send({
      ...toWithdrawalResponse(withdrawal),
      history: history.map(toWithdrawalStatusChangeResponse),
    });
  });
}
