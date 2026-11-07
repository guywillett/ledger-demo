import { randomUUID } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { buildSimApp, type SimDeps, type TransferUpdate } from '../../src/provider-sim/app.ts';
import { capturingLogger, FOREIGN_IBAN } from '../helpers.ts';

// provider-sim through app.inject: no network, no compose service. Time is injected: by default
// every wait resolves at once, so a transfer's result is delivered as soon as idle() is awaited.

const EXECUTION_DELAY_MS = 2000;
const SLOW_DELAY_MS = 10_000;

type Setup = {
  deps?: Partial<SimDeps>;
  // A wait of exactly this many ms never resolves (by default none).
  holdMs?: number;
};

function setup(options: Setup = {}) {
  const deliveries: TransferUpdate[] = [];
  const { logger, lines } = capturingLogger();
  const sim = buildSimApp({
    logger,
    now: () => new Date('2026-10-22T12:00:00.000Z'),
    random: () => 0.5,
    deliver: async (update) => {
      deliveries.push(update);
    },
    sleep: (ms) => (ms === options.holdMs ? new Promise<void>(() => {}) : Promise.resolve()),
    executionDelayMs: EXECUTION_DELAY_MS,
    slowDelayMs: SLOW_DELAY_MS,
    webhookMaxAttempts: 5,
    rates: { refuse: 0, error: 0, slow: 0, fail: 0, duplicate: 0, early: 0 },
    ...options.deps,
  });
  return { sim, deliveries, lines };
}

function body(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    amount_minor: '2500',
    currency: 'EUR',
    creditor_iban: FOREIGN_IBAN,
    creditor_name: 'Jane Doe',
    remittance_info: 'Rent October',
    reference: 'wd_01m5jjt69mec4t28vcqmrkmf79',
    ...overrides,
  };
}

async function submit(
  sim: ReturnType<typeof setup>['sim'],
  payload: Record<string, unknown>,
  // null: no Idempotency-Key header. (undefined would mean "use the default", a random key.)
  key: string | null = randomUUID(),
): Promise<{ status: number; json: Record<string, unknown> }> {
  const response = await sim.app.inject({
    method: 'POST',
    url: '/transfers',
    headers: {
      'content-type': 'application/json',
      ...(key === null ? {} : { 'idempotency-key': key }),
    },
    payload: JSON.stringify(payload),
  });
  return {
    status: response.statusCode,
    json: JSON.parse(response.body) as Record<string, unknown>,
  };
}

describe('POST /transfers', () => {
  it('creates a transfer, and reports it executed to the webhook', async () => {
    const { sim, deliveries } = setup();
    const created = await submit(sim, body());
    expect(created).toEqual({
      status: 201,
      json: { transfer_id: expect.stringMatching(/^tr_/), status: 'accepted' },
    });

    await sim.idle();
    expect(deliveries).toEqual([
      {
        provider_event_id: expect.stringMatching(/^pev_/),
        transfer_id: created.json.transfer_id,
        reference: 'wd_01m5jjt69mec4t28vcqmrkmf79',
        status: 'executed',
        occurred_at: '2026-10-22T12:00:00.000Z',
      },
    ]);
  });

  it('answers the same key and body with the same transfer, scheduling no second result', async () => {
    const { sim, deliveries } = setup();
    const key = randomUUID();
    const first = await submit(sim, body(), key);
    // Normalised the same way (IBAN spacing, trimmed name): the same request.
    const again = await submit(
      sim,
      body({ creditor_iban: 'de75 5121 0800 1245 1261 99', creditor_name: ' Jane Doe ' }),
      key,
    );
    expect([first.status, again.status]).toEqual([201, 200]);
    expect(again.json.transfer_id).toBe(first.json.transfer_id);

    await sim.idle();
    expect(deliveries).toHaveLength(1);
  });

  it('refuses the same key with another body: 409', async () => {
    const { sim } = setup();
    const key = randomUUID();
    await submit(sim, body(), key);
    expect((await submit(sim, body({ amount_minor: '2501' }), key)).status).toBe(409);
  });

  it.each([
    ['no Idempotency-Key', body(), null],
    ['an invalid IBAN', body({ creditor_iban: 'DE00512108001245126199' }), 'k'],
    ['a name outside the SEPA set', body({ creditor_name: 'Zoë' }), 'k'],
    ['a numeric amount', body({ amount_minor: 2500 }), 'k'],
    ['no reference', body({ reference: undefined }), 'k'],
  ] as const)('answers 400 for %s', async (_case, payload, key) => {
    const { sim, deliveries } = setup();
    expect((await submit(sim, payload, key)).status).toBe(400);
    await sim.idle();
    expect(deliveries).toEqual([]);
  });
});

describe('400', () => {
  it('lists each problem by path and message, never the input values', async () => {
    const { sim } = setup();
    const response = await submit(
      sim,
      body({ creditor_iban: 'DE00512108001245126199', creditor_name: 'Zoë Müller' }),
      null,
    );
    expect(response.status).toBe(400);
    expect(response.json.error).toBe('invalid transfer request');
    const errors = response.json.errors as { path: string; message: string }[];
    expect(errors.map((e) => e.path)).toEqual([
      'idempotency-key',
      'creditor_iban',
      'creditor_name',
    ]);
    expect(errors.every((e) => typeof e.message === 'string' && e.message !== '')).toBe(true);
    const text = JSON.stringify(response.json);
    expect(text).not.toContain('DE00512108001245126199');
    expect(text).not.toContain('Zoë');
  });
});

describe('triggers in remittance_info', () => {
  it('SIM REFUSE: 422 with a reason, and nothing stored', async () => {
    const { sim, deliveries } = setup();
    const key = randomUUID();
    const refused = await submit(sim, body({ remittance_info: 'SIM REFUSE closed' }), key);
    expect(refused).toEqual({ status: 422, json: { reason: 'creditor account closed' } });
    // Nothing stored: the same key is refused again, not answered as a repeat.
    expect((await submit(sim, body({ remittance_info: 'SIM REFUSE closed' }), key)).status).toBe(
      422,
    );
    await sim.idle();
    expect(deliveries).toEqual([]);
  });

  it('remembers a refusal per key: the same request is refused again, another body is a 409', async () => {
    // SIM_REFUSE_RATE 0.5 and a random() that refuses only on its first roll (0.1, then 0.9): a
    // retry must not re-roll its way to acceptance.
    const rolls = [0.1, 0.9, 0.9, 0.9];
    let calls = 0;
    const { sim, deliveries } = setup({
      deps: {
        rates: { refuse: 0.5, error: 0, slow: 0, fail: 0, duplicate: 0, early: 0 },
        random: () => rolls[calls++] ?? 0.9,
      },
    });
    const key = randomUUID();
    expect((await submit(sim, body(), key)).status).toBe(422);
    expect((await submit(sim, body(), key)).status).toBe(422);
    expect(calls).toBe(1);
    expect((await submit(sim, body({ amount_minor: '2501' }), key)).status).toBe(409);

    // Another key rolls afresh (0.9: accepted).
    expect((await submit(sim, body())).status).toBe(201);
    await sim.idle();
    expect(deliveries).toHaveLength(1);
  });

  it('SIM ERROR: 503 the first time, without creating the transfer; the retry gets 201', async () => {
    const { sim, deliveries } = setup();
    const key = randomUUID();
    const payload = body({ remittance_info: 'SIM ERROR' });
    expect((await submit(sim, payload, key)).status).toBe(503);
    await sim.idle();
    expect(deliveries).toEqual([]);

    expect((await submit(sim, payload, key)).status).toBe(201);
    await sim.idle();
    expect(deliveries).toHaveLength(1);
  });

  it('SIM SLOW: the transfer exists before the delayed answer, and a retry meanwhile gets 200', async () => {
    const { sim } = setup({ holdMs: SLOW_DELAY_MS });
    const key = randomUUID();
    const payload = body({ remittance_info: 'SIM SLOW' });

    // The first request waits forever (holdMs): the submitter's timeout. Not awaited.
    let firstAnswered = false;
    void submit(sim, payload, key).then(() => {
      firstAnswered = true;
    });
    // Let the first request run up to its wait.
    await new Promise((resolve) => setImmediate(resolve));

    const retry = await submit(sim, payload, key);
    expect(retry.status).toBe(200);
    expect(retry.json.transfer_id).toMatch(/^tr_/);
    expect(firstAnswered).toBe(false);
  });

  it('SIM FAIL: accepted, then reported failed with a reason', async () => {
    const { sim, deliveries } = setup();
    expect((await submit(sim, body({ remittance_info: 'SIM FAIL' }))).status).toBe(201);
    await sim.idle();
    expect(deliveries).toEqual([
      expect.objectContaining({ status: 'failed', reason: 'rejected by the beneficiary bank' }),
    ]);
  });

  it('SIM DUPLICATE: the result is delivered twice, with one provider_event_id', async () => {
    const { sim, deliveries } = setup();
    await submit(sim, body({ remittance_info: 'SIM DUPLICATE' }));
    await sim.idle();
    expect(deliveries).toHaveLength(2);
    expect(deliveries[1]).toEqual(deliveries[0]);
  });

  it('SIM EARLY: the result is delivered before the submission is answered', async () => {
    // The scheduled path never fires (its wait is held), so a delivery seen when the response
    // arrives can only have come before it.
    const { sim, deliveries } = setup({ holdMs: EXECUTION_DELAY_MS });
    const created = await submit(sim, body({ remittance_info: 'SIM EARLY' }));
    expect(created.status).toBe(201);
    expect(deliveries).toEqual([
      expect.objectContaining({ transfer_id: created.json.transfer_id }),
    ]);

    // With a failing receiver it still answers promptly: one attempt, then the retries carry on
    // in the background (the retry wait of 1000 ms is held, so they are still pending).
    let attempts = 0;
    const failing = setup({
      holdMs: 1000,
      deps: {
        deliver: async () => {
          attempts += 1;
          throw new Error('webhook answered 503');
        },
      },
    });
    expect((await submit(failing.sim, body({ remittance_info: 'SIM EARLY' }))).status).toBe(201);
    expect(attempts).toBe(1);
    expect(failing.sim.stop()).toBeGreaterThan(0);

    // Without the trigger, nothing is delivered by the time of the response.
    const normal = setup({ holdMs: EXECUTION_DELAY_MS });
    await submit(normal.sim, body());
    expect(normal.deliveries).toEqual([]);
  });

  it('random rates apply only without a trigger, and a trigger wins', async () => {
    const { sim, deliveries } = setup({
      deps: { rates: { refuse: 0, error: 0, slow: 0, fail: 0.6, duplicate: 0, early: 0 } },
    });
    // random() is 0.5 < 0.6: failed.
    await submit(sim, body());
    // A trigger overrides the rate: delivered twice, executed.
    await submit(sim, body({ remittance_info: 'SIM DUPLICATE' }));
    await sim.idle();
    expect(deliveries.map((d) => d.status).sort()).toEqual(['executed', 'executed', 'failed']);
  });
});

describe('webhook delivery', () => {
  it('retries until a 2xx: two failures, then success, is three attempts', async () => {
    let calls = 0;
    const { sim, lines } = setup({
      deps: {
        deliver: async () => {
          calls += 1;
          if (calls <= 2) throw new Error('webhook answered 503');
        },
      },
    });
    await submit(sim, body());
    await sim.idle();
    expect(calls).toBe(3);
    const attempts = lines().filter((l) => String(l.msg).startsWith('transfer result'));
    expect(attempts.map((l) => [l.msg, l.attempt])).toEqual([
      ['transfer result delivery failed', 1],
      ['transfer result delivery failed', 2],
      ['transfer result delivered', 3],
    ]);
  });

  it('gives up after the maximum, with an error log', async () => {
    let calls = 0;
    const { sim, lines } = setup({
      deps: {
        deliver: async () => {
          calls += 1;
          throw new Error('connection refused');
        },
      },
    });
    await submit(sim, body());
    await sim.idle();
    expect(calls).toBe(5);
    expect(lines().find((l) => l.msg === 'gave up delivering the transfer result')).toMatchObject({
      level: 'error',
      attempts: 5,
    });
  });

  it('sends nothing without a webhook URL, and logs the result instead', async () => {
    const { sim, lines } = setup({ deps: { deliver: undefined } });
    await submit(sim, body());
    await sim.idle();
    expect(
      lines().find((l) => l.msg === 'transfer result (not sent: SIM_WEBHOOK_URL unset)'),
    ).toMatchObject({ status: 'executed', reference: 'wd_01m5jjt69mec4t28vcqmrkmf79' });
  });

  it('logs no names, IBANs or remittance text', async () => {
    const { sim, lines } = setup();
    await submit(sim, body());
    await sim.idle();
    const everything = JSON.stringify(lines());
    for (const personal of [FOREIGN_IBAN, 'Jane Doe', 'Rent October']) {
      expect(everything).not.toContain(personal);
    }
  });
});

// GET /_test/transfers (slice 4 decision 73): what provider-sim has seen for a reference. Every
// test below submits with a random Idempotency-Key (submit's default) and reads by reference, so
// none relies on the two being the same.
describe('GET /_test/transfers', () => {
  const REFERENCE = 'wd_01m5jjt69mec4t28vcqmrkmf79'; // body()'s

  async function seen(
    sim: ReturnType<typeof setup>['sim'],
    query: Record<string, string | string[]> = { reference: REFERENCE },
  ): Promise<{ status: number; json: Record<string, unknown> }> {
    const response = await sim.app.inject({ method: 'GET', url: '/_test/transfers', query });
    return {
      status: response.statusCode,
      json: JSON.parse(response.body) as Record<string, unknown>,
    };
  }

  // The transfer as body() describes it, executed and reported once.
  function transferOf(transferId: unknown): Record<string, unknown> {
    return {
      transfer_id: transferId,
      amount_minor: '2500',
      currency: 'EUR',
      creditor_iban: FOREIGN_IBAN,
      creditor_name: 'Jane Doe',
      remittance_info: 'Rent October',
      outcome: 'executed',
      failure_reason: null,
      reports_twice: false,
    };
  }

  it('answers 404 for a reference no request has carried', async () => {
    const { sim } = setup();
    await submit(sim, body({ reference: 'wd_other' }));
    const response = await seen(sim);
    expect(response.status).toBe(404);
    expect(response.json).toEqual({ error: expect.any(String) });
  });

  it.each([
    ['no reference', {}],
    ['an empty reference', { reference: '' }],
    ['a reference over 35 characters', { reference: 'x'.repeat(36) }],
    ['a repeated reference', { reference: [REFERENCE, REFERENCE] }],
  ])('answers 400 for %s', async (_case, query) => {
    const { sim } = setup();
    await submit(sim, body());
    const response = await seen(sim, query);
    expect(response.status).toBe(400);
    expect(response.json.error).toEqual(expect.any(String));
  });

  it('gives a created transfer: one request, its fields as normalised, executed', async () => {
    const { sim } = setup();
    // Spaced, lower-case IBAN and a padded name: stored as normalised.
    const created = await submit(
      sim,
      body({ creditor_iban: 'de75 5121 0800 1245 1261 99', creditor_name: ' Jane Doe ' }),
    );
    expect(await seen(sim)).toEqual({
      status: 200,
      json: {
        reference: REFERENCE,
        requests: 1,
        transfer: transferOf(created.json.transfer_id),
        refused: false,
        errored: false,
      },
    });
  });

  it('gives remittance_info as null when the request had none', async () => {
    const { sim } = setup();
    await submit(sim, body({ remittance_info: undefined }));
    expect((await seen(sim)).json.transfer).toMatchObject({ remittance_info: null });
  });

  it('counts a repeat with the same key and body, the transfer unchanged', async () => {
    const { sim } = setup();
    const key = randomUUID();
    const created = await submit(sim, body(), key);
    await submit(sim, body(), key);
    const response = await seen(sim);
    expect(response.json).toMatchObject({
      requests: 2,
      transfer: transferOf(created.json.transfer_id),
    });
  });

  it('counts a 409 key conflict, the transfer unchanged', async () => {
    const { sim } = setup();
    const key = randomUUID();
    const created = await submit(sim, body(), key);
    expect((await submit(sim, body({ amount_minor: '2501' }), key)).status).toBe(409);
    expect((await seen(sim)).json).toMatchObject({
      requests: 2,
      transfer: transferOf(created.json.transfer_id),
    });
  });

  it("doesn't count a 400, whose body couldn't be read, nor one without an Idempotency-Key", async () => {
    const { sim } = setup();
    expect((await submit(sim, body(), null)).status).toBe(400);
    expect((await seen(sim)).status).toBe(404);

    await submit(sim, body());
    expect((await submit(sim, body({ creditor_iban: 'DE00512108001245126199' }))).status).toBe(400);
    expect((await seen(sim)).json.requests).toBe(1);
  });

  // A 409 carrying a new reference under another reference's key: counted for its own reference,
  // which has no transfer.
  it("doesn't show another reference's transfer for a 409 with a new reference", async () => {
    const { sim } = setup();
    const key = randomUUID();
    await submit(sim, body(), key);
    expect((await submit(sim, body({ reference: 'wd_other' }), key)).status).toBe(409);
    expect((await seen(sim, { reference: 'wd_other' })).json).toEqual({
      reference: 'wd_other',
      requests: 1,
      transfer: null,
      refused: false,
      errored: false,
    });
  });

  // The same for a 503: a 409 carrying B under a key whose first request (A) was answered 503
  // (then created on A's retry) doesn't make B errored.
  it("doesn't show another reference's 503 for a 409 with a new reference", async () => {
    const { sim } = setup();
    const key = randomUUID();
    const payload = body({ remittance_info: 'SIM ERROR' });
    expect((await submit(sim, payload, key)).status).toBe(503);
    expect((await submit(sim, payload, key)).status).toBe(201);
    expect((await submit(sim, { ...payload, reference: 'wd_other' }, key)).status).toBe(409);
    expect((await seen(sim, { reference: 'wd_other' })).json).toEqual({
      reference: 'wd_other',
      requests: 1,
      transfer: null,
      refused: false,
      errored: false,
    });
    expect((await seen(sim)).json).toMatchObject({ requests: 2, errored: true });
  });

  it('keeps each reference apart', async () => {
    const { sim } = setup();
    await submit(sim, body());
    await submit(sim, body());
    const other = await submit(sim, body({ reference: 'wd_other', amount_minor: '99' }));
    expect((await seen(sim)).json.requests).toBe(2);
    expect((await seen(sim, { reference: 'wd_other' })).json).toMatchObject({
      reference: 'wd_other',
      requests: 1,
      transfer: { transfer_id: other.json.transfer_id, amount_minor: '99' },
    });
  });

  it('SIM FAIL: outcome failed, with its reason', async () => {
    const { sim } = setup();
    await submit(sim, body({ remittance_info: 'SIM FAIL' }));
    expect((await seen(sim)).json.transfer).toMatchObject({
      outcome: 'failed',
      failure_reason: 'rejected by the beneficiary bank',
      reports_twice: false,
    });
  });

  it('SIM DUPLICATE: reports_twice', async () => {
    const { sim } = setup();
    await submit(sim, body({ remittance_info: 'SIM DUPLICATE' }));
    expect((await seen(sim)).json.transfer).toMatchObject({
      outcome: 'executed',
      reports_twice: true,
    });
  });

  it('SIM REFUSE: refused, no transfer, and the repeat counted', async () => {
    const { sim } = setup();
    const key = randomUUID();
    await submit(sim, body({ remittance_info: 'SIM REFUSE' }), key);
    expect((await seen(sim)).json).toEqual({
      reference: REFERENCE,
      requests: 1,
      transfer: null,
      refused: true,
      errored: false,
    });
    await submit(sim, body({ remittance_info: 'SIM REFUSE' }), key);
    expect((await seen(sim)).json).toMatchObject({ requests: 2, refused: true, transfer: null });
  });

  it('SIM ERROR: errored with no transfer, then created on the retry', async () => {
    const { sim } = setup();
    const key = randomUUID();
    const payload = body({ remittance_info: 'SIM ERROR' });
    await submit(sim, payload, key);
    expect((await seen(sim)).json).toEqual({
      reference: REFERENCE,
      requests: 1,
      transfer: null,
      refused: false,
      errored: true,
    });
    const created = await submit(sim, payload, key);
    expect((await seen(sim)).json).toMatchObject({
      requests: 2,
      transfer: { transfer_id: created.json.transfer_id, outcome: 'executed' },
      errored: true,
    });
  });

  it('SIM SLOW: the transfer and its request are seen while the answer is still delayed', async () => {
    const { sim } = setup({ holdMs: SLOW_DELAY_MS });
    void submit(sim, body({ remittance_info: 'SIM SLOW' })); // never answered (holdMs)
    // Until the request has reached its handler (inject takes a few turns of the event loop).
    let response = await seen(sim);
    for (let turns = 0; response.status === 404 && turns < 100; turns++) {
      await new Promise((resolve) => setImmediate(resolve));
      response = await seen(sim);
    }
    expect(response.json).toMatchObject({
      requests: 1,
      transfer: { transfer_id: expect.stringMatching(/^tr_/) },
    });
  });
});
