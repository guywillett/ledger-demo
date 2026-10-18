# ledger-demo

Portfolio project: a small payment event-processing system (incoming payments, pseudo compliance review, ledger balances) built to demonstrate correctness under duplicates, retries, crashes and concurrency. Implemented first in Node.js/TypeScript, later in Go against the same database schema and contracts.

## Specs

- Work is done in slices. The current slice spec is the source of truth: `docs/slices/slice-1-incoming-payments.md`.
- Design decisions live in the spec's *Decisions* section and in `docs/decisions/`. Don't contradict them silently: if something in a spec looks wrong or unworkable, stop and raise it.

## How we work

- The owner is a senior backend engineer (Python/Django background) learning idiomatic Node/TypeScript and Go through this project. Understanding every step is half the point.
- Work in small steps: one coherent piece at a time (e.g. compose + migrations, then one endpoint), not the whole slice at once.
- Before writing code for a step, say briefly what you're about to do and why. After it, summarise what changed.
- Explain non-obvious language or library idioms when you first use them (Node event loop, promise handling, TypeScript types vs runtime validation, etc.). Skip what a senior backend engineer already knows (SQL, transactions, HTTP).
- Ask rather than guess when a choice isn't covered by the spec.
- Don't commit unless asked. Feature branches; history gets tidied before merging to `main`.

## Conventions

- **SQL:** raw parameterised SQL via `pg` (`$1`, `$2`), no ORM, no query builder. All SQL lives in the data-access layer, never in route handlers. Explicit typed row mapping.
- **No check-then-act:** never `SELECT` to decide whether something has happened and then act. Use conditional `UPDATE … WHERE status = $expected`, `INSERT … ON CONFLICT DO NOTHING`, `rowCount` and `RETURNING`.
- **Balances** change only through `ledger/move()`. The API never changes balances.
- **IDs:** UUIDv7 in `uuid` columns; TypeIDs (`typeid-js`, prefixes `acct`, `pay`, `evt`) outside the database. Convert only at boundaries. Never store IDs as text.
- **Money:** integer minor units (`bigint`), never floats.
- **Validation:** Zod at every boundary (HTTP bodies, NATS messages).
- **Migrations:** plain SQL in `db/migrations/`, run by dbmate. Language-neutral (shared with Go later).
- **Versions:** pin Docker image tags and dependency versions; commit lockfiles.
- **Logging:** structured JSON with `request_id`, `payment_id`, `event_id` where relevant.

## Running

- `docker compose up` starts Postgres, NATS (JetStream), migrations, API and worker.
- Integration tests (Vitest) run against the compose services.