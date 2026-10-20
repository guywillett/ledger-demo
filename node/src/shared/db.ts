import pg from 'pg';
import type { Logger } from './logger.ts';

// A connection checked out of the pool with a transaction open on it.
// Data-access functions that must run inside a transaction take a Tx.
export type Tx = pg.PoolClient;

// Anything that can run a query: the pool (each query on whichever connection is free) or a
// Tx. Data-access functions whose work is a single statement take a Queryable, so callers can
// run them on their own or inside a transaction.
export type Queryable = Pick<pg.Pool, 'query'>;

// pg returns bigint (int8) columns as strings, because a JS number cannot hold every
// 64-bit integer. Parse them as BigInt: exact across the whole int8 range, and BigInt
// can't be mixed with number, so float arithmetic on money is a TypeError.
// Side effect: COUNT(*) is int8 too, so cast counts in SQL (COUNT(*)::int) when a
// number is wanted. See docs/decisions/0001-money-representation.md.
export function parseInt8(value: string): bigint {
  return BigInt(value);
}

// Per-pool type parsers, so importing this module doesn't change pg's global defaults.
const types = new pg.TypeOverrides();
types.setTypeParser(pg.types.builtins.INT8, parseInt8);

export function createPool(options: { databaseUrl: string; logger: Logger }): pg.Pool {
  const pool = new pg.Pool({ connectionString: options.databaseUrl, types });
  // An idle connection can fail (e.g. Postgres restarts). The pool emits 'error', and an
  // 'error' event with no listener crashes the process, so log it; the pool replaces the connection.
  pool.on('error', (err) => {
    options.logger.error({ err }, 'idle database connection error');
  });
  return pool;
}

// Runs `fn` in a transaction on a single connection: COMMIT if it resolves,
// ROLLBACK (and rethrow) if it throws.
export async function withTransaction<T>(pool: pg.Pool, fn: (tx: Tx) => Promise<T>): Promise<T> {
  const client = await pool.connect();
  let broken: Error | undefined;
  try {
    await client.query('BEGIN');
    const result = await fn(client);
    await client.query('COMMIT');
    return result;
  } catch (err) {
    try {
      await client.query('ROLLBACK');
    } catch (rollbackErr) {
      // The connection is in an unknown state; passing an error to release() destroys it
      // instead of returning it to the pool.
      broken = rollbackErr instanceof Error ? rollbackErr : new Error(String(rollbackErr));
    }
    throw err;
  } finally {
    client.release(broken);
  }
}
