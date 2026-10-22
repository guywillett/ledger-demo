# 0001 — Money representation

Status: accepted (slice 1)

## Context

Amounts are integer minor units (cents) in a single currency (EUR). Postgres stores them as `bigint` (`int8`).

The `pg` driver returns `int8` as a **string**, because a JavaScript `number` (an IEEE 754 double) is exact only up to 2^53 − 1. The Node ecosystem has no single convention: `pg`, Knex, Kysely, TypeORM and Sequelize return strings by default; Drizzle makes you choose per column (`mode: 'number' | 'bigint'`); Prisma returns native `BigInt`. Go has no such problem: `int64` maps onto `bigint` exactly.

We first parsed `int8` as `number` with a safe-integer guard. That is correct for EUR (2^53 cents is about €90 trillion), but it only guards the database boundary, not arithmetic done in JavaScript, and it allows accidental float arithmetic on money.

## Decision

- **Postgres:** keep `bigint` for amounts.
- **Node:** `int8` columns are parsed as `BigInt` (a per-pool type parser in `shared/db.ts`). Amounts are `bigint` in TypeScript throughout.
- **JSON (API and events):** amounts are **strings of digits**, e.g. `"amount_minor": "1250"`. `JSON.stringify` cannot serialise `BigInt`, and strings also protect JavaScript clients from precision loss. Inputs are validated with Zod as a digit string and converted to `BigInt`.
- **Logs:** follow the same rule. pino would write a `BigInt` as a bare JSON number (rounded above 2^53 by most JSON parsers), so `shared/logger.ts` converts `BigInt` values in the log call's object to digit strings. pino skips formatters for child bindings, so amounts go in the log call's object, never in `logger.child({ … })`.
- **Counts:** because the parser is per column type, `COUNT(*)` (which is `int8`) would also arrive as `BigInt`. Cast counts in SQL (`COUNT(*)::int`) where a `number` is wanted. `bigserial` IDs arrive as `BigInt`; they are internal and rarely read.
- **Go (later):** `int64` for amounts, the same JSON string format.

## Consequences

- Mixing `bigint` and `number` is a `TypeError`, so float arithmetic on money (`amount * 1.2`) cannot happen by accident.
- Node and Go agree on every value in the `bigint` range, not just those below 2^53.
- Every JSON boundary needs explicit serialisation of amounts.

## Rounding and division

Addition and subtraction of minor units are exact. `BigInt` division truncates towards zero (`-7n / 2n === -3n`), unlike Python's `//` (floors) and with no rounding modes like `Decimal`. Slice 1 does no division. When it is needed:

- all division goes through one helper with an **explicit rounding mode** (e.g. half-even);
- splitting an amount uses **allocation** (e.g. 1000 cents three ways → 334 + 333 + 333, remainder distributed explicitly), so the parts always sum to the whole;
- heavier decimal work uses a decimal library with rounding modes (`decimal.js` / `big.js` in Node, `shopspring/decimal` in Go) or Postgres `numeric`.

## Not now, but noted

- **Crypto amounts** (e.g. 18-decimal tokens) overflow `int64` (max ≈ 9.2 × 10^18 wei ≈ 9.2 ETH). They would need `numeric(78,0)` in Postgres, `BigInt` in Node and `big.Int` in Go.
- **Rates are not money** (fees, FX): store as `numeric`, carry as decimal strings, compute in a decimal library, round to minor units once at the end with an explicit mode, and record the rate and rounding used alongside the resulting movement.
