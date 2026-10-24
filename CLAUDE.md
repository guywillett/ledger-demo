# ledger-demo

Portfolio project: a small payment event-processing system (incoming payments, pseudo compliance review, ledger balances) built to demonstrate correctness under duplicates, retries, crashes and concurrency. Implemented first in Node.js/TypeScript, later in Go against the same database schema and contracts.

## Specs

- Work is done in slices. The current slice spec is the source of truth: `docs/slices/slice-2-outbox-and-withdrawals.md`.
- Design decisions live in the spec's *Decisions* section and in `docs/decisions/`. Don't contradict them silently: if something in a spec looks wrong or unworkable, stop and raise it.

## How we work

- The owner is a senior backend engineer (Python/Django background) learning idiomatic Node/TypeScript and Go through this project. Understanding every step is half the point.
- Work in small steps: one coherent piece at a time (e.g. compose + migrations, then one endpoint), not the whole slice at once.
- Before writing code for a step, say briefly what you're about to do and why. After it, summarise what changed.
- Explain non-obvious language or library idioms when you first use them (Node event loop, promise handling, TypeScript types vs runtime validation, etc.). Skip what a senior backend engineer already knows (SQL, transactions, HTTP).
- Ask rather than guess when a choice isn't covered by the spec.
- Don't commit unless asked. At the end of each step, propose a commit message and wait for the go-ahead.
- **Branching:** one branch per slice (`slice-1`, `slice-2`, …) off `main`, with one commit per step. Short-lived side branches only for experiments that may be discarded. When the slice is done, tidy its history and merge into `main` with `--no-ff` — no squash, no fast-forward — so `git log --first-parent main` shows one merge per slice and the step commits are kept. Release tags (e.g. `v0.1-simple-ledger`) go on those merge commits.
- **Fix-ups:** when a change belongs to an earlier step, commit it with `git commit --fixup=<step-sha>` rather than as a loose "fix" commit. The owner folds them in at the end with `git rebase -i --autosquash main` (don't run interactive rebases yourself). Only for steps on the current slice branch; a fix to an already-merged slice is a normal commit.
- **Pushing:** the slice branch may be rebased during tidy-up, so pushing it afterwards needs `git push --force-with-lease`. Never rebase or force-push `main`.
- **Commit messages:** imperative summary line (≤ 72 chars), blank line, then a short body saying why, not just what.

## Conventions

- **SQL:** raw parameterised SQL via `pg` (`$1`, `$2`), no ORM, no query builder. All SQL lives in the data-access layer, never in route handlers. Explicit typed row mapping.
- **No check-then-act:** never `SELECT` to decide whether something has happened and then act. Use conditional `UPDATE … WHERE status = $expected`, `INSERT … ON CONFLICT DO NOTHING`, `rowCount` and `RETURNING`.
- **Result vocabularies:** conditional transitions return `'applied' | 'skipped'` (skipped: not in the expected state); idempotent inserts return `'applied' | 'duplicate'` (duplicate: already recorded). Name new results the same way.
- **Balances** change only through `ledger/move()`, called by the ledger's posting functions. The API calls a posting function only inside its own request transaction, and only when the response depends on the outcome (slice 2 decision 25: holding a withdrawal's funds); everything else is posted asynchronously (the worker).
- **Transactions:** callers own transaction boundaries via `withTransaction`; data-access functions never `BEGIN`/`COMMIT`. A function whose statements must be atomic together takes a `Tx` (branded: only `withTransaction` creates one); a single-statement function takes a `Queryable` (pool or `Tx`). Never call `withTransaction` inside another (that opens a second, independent transaction), and never wait on the network (NATS, HTTP) inside one.
- **IDs:** UUIDv7 in `uuid` columns; TypeIDs (`typeid-js`, prefixes `acct`, `pay`, `wd`, `evt`, `mov`) outside the database. Convert only at boundaries. Never store IDs as text.
- **Money:** integer minor units: `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON (`"amount_minor": "1250"`). Never floats. Cast counts to `::int` in SQL. Any division goes through one helper with an explicit rounding mode. See `docs/decisions/0001-money-representation.md`.
- **Validation:** Zod at every boundary (HTTP bodies, NATS messages).
- **Status codes follow the caller** (slice 2 decision 38): the client API (top of `src/api/`) uses conventional REST codes; provider webhooks live under `src/api/webhooks/` at `/provider/…` and follow the webhook contract (`204` for anything recorded or deliberately handled, `400` only for an unusable body, `500` when a retry can help).
- **Migrations:** plain SQL in `db/migrations/`, run by dbmate. Language-neutral (shared with Go later).
- **Versions:** pin Docker image tags and dependency versions; commit lockfiles.
- **Logging:** structured JSON with `request_id`, `payment_id`, `event_id` where relevant.

## Running

- `docker compose up` starts Postgres, NATS (JetStream), migrations, API, relay and worker.
- Integration tests (Vitest) run against the compose services.