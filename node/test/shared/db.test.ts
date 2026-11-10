import { setTimeout as sleep } from 'node:timers/promises';
import { afterAll, describe, expect, it } from 'vitest';
import { loadSharedConfig } from '../../src/shared/config.ts';
import type pg from 'pg';
import { afterCommit, createPool, ping, withTransaction, type Tx } from '../../src/shared/db.ts';
import { createLogger } from '../../src/shared/logger.ts';
import { balancesOf, capturingLogger, createTestAccount } from '../helpers.ts';

// Runs against the compose Postgres (docker compose up -d).
const config = loadSharedConfig();
const pool = createPool({
  databaseUrl: config.databaseUrl,
  logger: createLogger({ service: 'test', level: config.logLevel }),
});

afterAll(async () => {
  await pool.end();
});

describe('database', () => {
  // Other tests create their own accounts in the same database, so check that the seed
  // accounts are present, not that they are the only ones.
  it('sees the seeded demo accounts', async () => {
    const result = await pool.query<{ name: string }>(
      'SELECT name FROM accounts WHERE name = ANY($1) ORDER BY name',
      [['alice', 'bob', 'carol']],
    );
    expect(result.rows.map((r) => r.name)).toEqual(['alice', 'bob', 'carol']);
  });

  // createTestAccount inserts only the account; the accounts_create_balance trigger adds the row.
  it('creates a zero balance row with every account', async () => {
    const account = await createTestAccount(pool);
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 0n, available: 0n, held: 0n });
  });

  it('returns bigint columns as BigInt', async () => {
    const result = await pool.query<{ n: bigint }>('SELECT $1::bigint AS n', ['1250']);
    expect(result.rows[0]?.n).toBe(1250n);
  });

  it('keeps bigint values beyond the safe integer range exact', async () => {
    const result = await pool.query<{ max: bigint; min: bigint }>(
      'SELECT 9223372036854775807::bigint AS max, (-9223372036854775807 - 1)::bigint AS min',
    );
    expect(result.rows[0]?.max).toBe(9223372036854775807n);
    expect(result.rows[0]?.min).toBe(-9223372036854775808n);
  });

  it('accepts BigInt query parameters', async () => {
    const result = await pool.query<{ n: bigint }>('SELECT $1::bigint + 1 AS n', [
      9007199254740993n,
    ]);
    expect(result.rows[0]?.n).toBe(9007199254740994n);
  });

  it('returns counts as numbers when cast to int', async () => {
    const result = await pool.query<{ n: number }>('SELECT COUNT(*)::int AS n FROM accounts');
    expect(typeof result.rows[0]?.n).toBe('number');
  });

  it('commits when the function resolves', async () => {
    const value = await withTransaction(pool, async (tx) => {
      const result = await tx.query<{ v: bigint }>('SELECT 42::bigint AS v');
      return result.rows[0]?.v;
    });
    expect(value).toBe(42n);
  });

  it('runs the function inside one transaction', async () => {
    // Every statement in a transaction sees the same transaction ID; in autocommit each
    // statement would get its own.
    const [first, second] = await withTransaction(pool, async (tx) => {
      const sql = 'SELECT pg_current_xact_id()::text AS id';
      const a = await tx.query<{ id: string }>(sql);
      const b = await tx.query<{ id: string }>(sql);
      return [a.rows[0]?.id, b.rows[0]?.id];
    });
    expect(first).toBeDefined();
    expect(second).toBe(first);
  });

  // Compile-time only: tsc fails if the @ts-expect-error line does NOT error. The function is
  // never called.
  it('accepts only a Tx from withTransaction where one is required', () => {
    const needsTx = (tx: Tx): string => typeof tx;
    const misuse = async (): Promise<void> => {
      const client = await pool.connect();
      // @ts-expect-error: a raw PoolClient has no transaction open, so it is not a Tx
      needsTx(client);
      client.release();
    };
    expect(typeof misuse).toBe('function');
  });

  it('rolls back when the function throws', async () => {
    const name = `rollback-test-${Date.now()}`;
    await expect(
      withTransaction(pool, async (tx) => {
        await tx.query('INSERT INTO accounts (name, iban) VALUES ($1, $2)', [
          name,
          'FR7612345000010000000009990',
        ]);
        throw new Error('boom');
      }),
    ).rejects.toThrow('boom');

    const result = await pool.query('SELECT 1 FROM accounts WHERE name = $1', [name]);
    expect(result.rowCount).toBe(0);
  });
});

// ping (slice 4 decision 72): through the pool, and after its deadline it ends what pg lets it end.
// The timing cases use a stub pool: a connection handed over late, or a query that never answers,
// are hard to arrange on a real database.
// The after-commit hooks (slice 5): what runs only once a transaction has committed.
describe('afterCommit', () => {
  it('runs the hooks after COMMIT, in the order queued', async () => {
    const ran: string[] = [];
    const result = await withTransaction(pool, async (tx) => {
      afterCommit(tx, () => ran.push('first'));
      afterCommit(tx, () => ran.push('second'));
      await tx.query('SELECT 1');
      expect(ran).toEqual([]); // not before the commit
      return 'done';
    });
    expect(result).toBe('done');
    expect(ran).toEqual(['first', 'second']);
  });

  it('never runs them after a rollback', async () => {
    const ran: string[] = [];
    await expect(
      withTransaction(pool, async (tx) => {
        afterCommit(tx, () => ran.push('hook'));
        throw new Error('rolled back');
      }),
    ).rejects.toThrow('rolled back');
    expect(ran).toEqual([]);
  });

  it('never runs them when COMMIT itself fails', async () => {
    const ran: string[] = [];
    await expect(
      withTransaction(pool, async (tx) => {
        afterCommit(tx, () => ran.push('hook'));
        // A deferred constraint violation surfaces at COMMIT, failing it.
        await tx.query(
          'CREATE TEMP TABLE t (id int PRIMARY KEY DEFERRABLE INITIALLY DEFERRED) ON COMMIT DROP',
        );
        await tx.query('INSERT INTO t VALUES (1), (1)');
      }),
    ).rejects.toMatchObject({ code: '23505' });
    expect(ran).toEqual([]);
  });

  it('logs a hook that throws, and the transaction and the later hooks stand', async () => {
    const { logger, lines } = capturingLogger();
    const own = createPool({ databaseUrl: config.databaseUrl, logger });
    try {
      const ran: string[] = [];
      const result = await withTransaction(own, async (tx) => {
        afterCommit(tx, () => {
          throw new Error('a broken hook');
        });
        afterCommit(tx, () => ran.push('after'));
        return 'committed';
      });
      expect(result).toBe('committed');
      expect(ran).toEqual(['after']);
      expect(lines()).toContainEqual(
        expect.objectContaining({
          level: 'error',
          msg: 'after-commit hook failed; the transaction stands',
        }),
      );
    } finally {
      await own.end();
    }
  });
});

describe('ping', () => {
  // A stub pool whose connect resolves after connectMs with a client whose query never answers
  // (queryHangs) or answers at once; it records what was done with the client.
  function stubPool(options: { connectMs: number; queryHangs: boolean }) {
    const record = { queries: 0, releases: [] as (Error | boolean | undefined)[] };
    const client = {
      query: () => {
        record.queries++;
        return options.queryHangs ? new Promise(() => undefined) : Promise.resolve({ rows: [] });
      },
      release: (err?: Error | boolean) => {
        record.releases.push(err);
      },
    };
    const stub = {
      connect: async () => {
        await sleep(options.connectMs);
        return client;
      },
    };
    return { pool: stub as unknown as pg.Pool, record };
  }

  it('resolves when the database answers', async () => {
    await expect(ping(pool, 1000)).resolves.toBeUndefined();
  });

  it('gives a connection that arrives after the deadline straight back, unused', async () => {
    const { pool: slow, record } = stubPool({ connectMs: 100, queryHangs: false });
    await expect(ping(slow, 20)).rejects.toThrow('did not answer within 20 ms');
    await sleep(150);
    expect(record).toEqual({ queries: 0, releases: [undefined] });
  });

  it('destroys a connection whose query outlives the deadline', async () => {
    const { pool: hung, record } = stubPool({ connectMs: 0, queryHangs: true });
    await expect(ping(hung, 20)).rejects.toThrow('did not answer within 20 ms');
    // Released with an error: the pool destroys the connection instead of reusing it.
    expect(record.queries).toBe(1);
    expect(record.releases).toEqual([expect.any(Error)]);
  });

  it('rejects at once while an earlier ping on the pool is unfinished, then runs again', async () => {
    const { pool: slow, record } = stubPool({ connectMs: 100, queryHangs: false });
    await expect(ping(slow, 20)).rejects.toThrow('did not answer');
    // The first one's connection hasn't arrived yet: no second attempt.
    await expect(ping(slow, 20)).rejects.toThrow('the previous check has not finished');
    await sleep(150); // the late connection arrives and goes back
    await expect(ping(slow, 500)).resolves.toBeUndefined();
    expect(record).toEqual({ queries: 1, releases: [undefined, undefined] });
  });
});
