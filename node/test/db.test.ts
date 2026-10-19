import { afterAll, describe, expect, it } from 'vitest';
import { loadSharedConfig } from '../src/shared/config.ts';
import { createPool, withTransaction } from '../src/shared/db.ts';
import { createLogger } from '../src/shared/logger.ts';

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
  it('sees the seeded demo accounts', async () => {
    const result = await pool.query<{ name: string }>('SELECT name FROM accounts ORDER BY name');
    expect(result.rows.map((r) => r.name)).toEqual(['alice', 'bob', 'carol']);
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
