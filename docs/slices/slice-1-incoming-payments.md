# Slice 1 — Incoming payments with pseudo approval

Status: agreed — ready to build
Implementation: Node.js / TypeScript (Go to follow in a later slice)

## Goal

The smallest end-to-end, runnable system that accepts incoming payments, holds them as pending, approves or rejects them asynchronously, and **provably changes balances exactly once**, however many times a request or event is repeated.

## In scope

- Repo skeleton and `docker compose up` for the whole system
- Postgres schema and migrations
- Payment API (create payment, read balance, read transaction history)
- Compliance worker consuming events from NATS JetStream
- Idempotency at the API (`network` + `network_transaction_id`), in the workflow (state-guarded transitions) and in the ledger (unique movement references)
- Delivery log for counting duplicates
- Reconciliation script
- Integration tests for the core invariants

## Out of scope (later slices)

- Withdrawals and held funds
- Transactional outbox (see *Known gaps*)
- Splitting compliance and ledger posting into separate consumers (needs the outbox first; the module boundary is in place from this slice)
- Dashboard, CI, load testing, Go implementation
- Double-entry ledger
- Payer details beyond the minimum, multiple currencies or networks

## Stack

| Concern | Choice | Notes |
|---|---|---|
| Runtime | Node.js (current LTS), TypeScript, ESM | strict mode |
| HTTP | Fastify | |
| Validation | Zod | runtime validation at every boundary (HTTP and events) |
| Database | PostgreSQL (pinned major version) | `pg` driver, plain SQL, no ORM |
| Migrations | dbmate (container) | plain SQL, language-neutral, shared with Go later |
| IDs | UUIDv7 + TypeID (`typeid-js`) | `uuid` in the DB; prefixed TypeID in API, events and logs |
| Messaging | NATS with JetStream (pinned version) | durable stream, explicit acks |
| Tests | Vitest | integration tests run against compose services |

All image tags and dependency versions pinned; lockfile committed.

## Repo layout (this slice)

```text
ledger-demo/
├── README.md
├── docker-compose.yml
├── contracts/
│   ├── openapi.yaml
│   └── events/payment.received.v1.json
├── db/migrations/
├── node/
│   ├── package.json
│   ├── src/
│   │   ├── api/          # Fastify app
│   │   ├── worker/       # consumer: wires compliance decision to ledger effects
│   │   ├── compliance/   # decides approve/reject — knows nothing about balances
│   │   ├── ledger/       # state transitions + move() — the only code that touches balances
│   │   └── shared/       # config, db pool, nats client, logging
│   ├── scripts/reconcile.ts
│   └── test/
└── docs/
    ├── slices/slice-1-incoming-payments.md
    └── decisions/
```

## Data model

Amounts are **integer minor units** (cents): `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON. No floats anywhere. See `docs/decisions/0001-money-representation.md`.

**IDs:** entity primary keys and event IDs are native `uuid` columns holding UUIDv7 (time-ordered, generated in the app). Every UUID column has `CHECK (uuid_extract_version(col) = 7)`; entity primary keys also have `DEFAULT uuidv7()` as a fallback for manual inserts only — the app always supplies IDs. Event IDs and references have no default. **Primary key rule:** UUIDv7 for entities that may be referenced or exposed (accounts, payments, balance movements); `bigserial` for internal append-only logs that are never referenced or exposed (`payment_status_history`, `event_deliveries`). Outside the database — API, events, logs — they appear as TypeIDs with a type prefix (`acct_…`, `pay_…`, `evt_…`). Conversion happens only in the data-access layer and the request/event schemas. Never store IDs as text or cast them in queries.

### `accounts`
| Column | Type | Notes |
|---|---|---|
| id | uuid PK | TypeID prefix `acct` |
| name | text | `UNIQUE`; human-readable label for demo accounts (e.g. `alice`) |
| created_at | timestamptz | |

Demo accounts are created by a dedicated seed migration (zero balances). There is no account-creation endpoint in this slice.

### `account_balances`
One row per account.
| Column | Type | Notes |
|---|---|---|
| account_id | uuid PK, FK | |
| pending_in | bigint | `CHECK (pending_in >= 0)` |
| available | bigint | `CHECK (available >= 0)` |
| updated_at | timestamptz | |

### `incoming_payments`
| Column | Type | Notes |
|---|---|---|
| id | uuid PK | TypeID prefix `pay` |
| account_id | uuid FK | |
| amount_minor | bigint | `CHECK (amount_minor > 0)` |
| currency | text | `CHECK (currency = 'EUR')` |
| network | text | `CHECK (network = 'SEPA')` |
| network_transaction_id | text | ID from the network / BaaS provider; `UNIQUE (network, network_transaction_id)` — the idempotency key for incoming payments |
| status | text | `received`, `under_review`, `approved`, `rejected` |
| request_hash | text | hash of the notification body, to detect the same network ID arriving with different details |
| notification_count | int | starts at 1; incremented on each duplicate notification with the same details (ingestion-side duplicate count) |
| created_at / updated_at | timestamptz | |

### `payment_status_history`
| Column | Type | Notes |
|---|---|---|
| id | bigserial PK | |
| payment_id | uuid FK | |
| from_status | text null | null for the first row |
| to_status | text | |
| reason | text null | e.g. rejection reason |
| at | timestamptz | |

### `balance_movements`
One row per balance movement, written only by `move()`. The ledger's own idempotency guard and audit trail; grows into the double-entry transactions table later.
| Column | Type | Notes |
|---|---|---|
| id | uuid PK | UUIDv7 |
| account_id | uuid FK | |
| from_bucket | text | `external`, `pending_in`, `available` |
| to_bucket | text | same set; `CHECK (from_bucket <> to_bucket)` |
| amount_minor | bigint | `CHECK (amount_minor > 0)` |
| reference_type | text | what `reference_id` is: `payment` (later `withdrawal`); also determines its TypeID prefix |
| reference_id | uuid | the business object causing the movement (here, the payment ID); not a foreign key |
| reference_step | text | the step within it: `review_start`, `approve`, `reject` |
| created_at | timestamptz | |

`UNIQUE (reference_type, reference_id, reference_step)` — a given movement can be applied once, whatever the caller does.

### `event_deliveries`
A log, not a guard: one row per message the worker receives. **No unique constraint on `event_id`** — recording repeated deliveries of the same event is the point.
| Column | Type | Notes |
|---|---|---|
| id | bigserial PK | internal log, never referenced or exposed |
| event_id | uuid | business event ID (TypeID prefix `evt`); indexed, not unique |
| nats_msg_id | text null | transport message ID |
| delivery_attempt | int | from JetStream message metadata |
| outcome | text | `in_progress`, `applied`, `duplicate`, `invalid`, `failed` |
| received_at | timestamptz | |
| completed_at | timestamptz null | |

## State machine

```text
received ──► under_review ──► approved
                         └──► rejected
```

Every transition is a conditional update (`UPDATE … WHERE id = $1 AND status = $expected`). If zero rows change, the transition has already happened and the operation is a no-op. Payment state **drives the workflow**; the ledger protects itself independently via unique movement references (see *The `move()` function*). Two guards, each owned by its own module.

Balance effects:

| Transition | Movement | `reference_step` |
|---|---|---|
| (create) → received | none — notified, not yet booked | — |
| received → under_review | external → `pending_in` | `review_start` |
| under_review → approved | `pending_in` → `available` | `approve` |
| under_review → rejected | `pending_in` → external (returned to sender) | `reject` |

`under_review` is set by the worker, not the API: a payment is only under review once the compliance process has actually picked it up. **The API never changes balances**; every balance change happens in the worker, through the ledger module.

Returning a rejected payment to the sender is modelled only as the ledger movement; the actual return over the network is out of scope.

### Concurrency rules (no check-then-act)

Correctness under concurrent or duplicate delivery comes from the database, never from reading state and deciding in code:

- **Never** `SELECT` to see whether an event, transition or movement has happened and then act on the result.
- Check the affected row count of every conditional transition.
- Call `move()` and write the history row **only** when the transition affected exactly one row, in the same transaction.
- `move()` inserts its movement with `ON CONFLICT DO NOTHING` and touches balances only if a row was inserted.
- Database backstops: conditional updates (Postgres re-evaluates the `WHERE` after waiting for a row lock), `UNIQUE (reference_type, reference_id, reference_step)` on movements, and `CHECK (… >= 0)` on balances.

## The `move()` function

All balance changes go through one function, called inside the caller's DB transaction:

```ts
move(tx, { accountId, from, to, amountMinor, referenceType, referenceId, referenceStep })
  : Promise<'applied' | 'duplicate'>
// from/to: 'external' | 'pending_in' | 'available'
```

1. `INSERT INTO balance_movements … ON CONFLICT (reference_type, reference_id, reference_step) DO NOTHING`.
2. If no row was inserted → return `'duplicate'`; balances untouched.
3. Otherwise update the `account_balances` columns for the non-external side(s) and return `'applied'`.

Nothing else may update balances. Because the guard lives in the ledger, `move()` is safe whatever the caller does — which is what lets compliance and posting become separate services later without relying on each other's state. If a transition succeeds but `move()` reports a duplicate, something is inconsistent: log an error and roll back.

`balance_movements` is the seam where double-entry arrives later: each movement becomes a transaction with balancing entries, keeping the same unique reference.

## API

### `POST /payments`
Models an **inbound notification from the network / BaaS provider** (webhook-style), not a client request.

Body: `{ network_transaction_id, account_id, amount_minor, currency: "EUR", network: "SEPA" }`, with `amount_minor` a string of digits (e.g. `"1250"`), validated and converted to `BigInt`.

One DB transaction: insert the payment (`received`) and its first history row. **No balance change** — ingestion only.

Then publish `payment.received` to JetStream with `Nats-Msg-Id = event_id`.

Idempotency behaviour (keyed on `network` + `network_transaction_id`):
- Same network ID, same details → `200` with the existing payment; `notification_count` incremented; no other effects
- Same network ID, different details → `409` and a warning log (provider bug or suspicious — not a retry)
- Concurrent notifications with the same network ID → exactly one creates; the rest get the existing payment (unique constraint, then re-read)

A client-supplied `Idempotency-Key` header is not used here; it arrives with withdrawals, which are genuinely client-initiated.

Response `201`: the payment, including status.

### `GET /accounts/:id/balance`
`{ pending_in, available, currency }`, with balances as digit strings (e.g. `{ "pending_in": "1250", "available": "0", "currency": "EUR" }`).

### `GET /accounts/:id/transactions`
Payments, newest first, each with current status. Optional `?include=history` adds the status-history rows.

## Event: `payment.received` v1

```json
{
  "event_id": "evt_…",
  "event_type": "payment.received",
  "schema_version": 1,
  "payment_id": "pay_…",
  "network": "SEPA",
  "network_transaction_id": "…",
  "account_id": "acct_…",
  "amount_minor": "1250",
  "currency": "EUR",
  "occurred_at": "2026-10-18T09:00:00Z"
}
```

`event_id` identifies the business event; the NATS message ID is transport only. The JSON schema lives in `contracts/events/`.

## Compliance worker

Durable JetStream consumer with explicit acks. For each message:

1. **Log the delivery** in its own small transaction: insert into `event_deliveries` with outcome `in_progress`.
2. Validate with Zod; invalid → outcome `invalid`, log and terminate the message (no infinite redelivery). A dead-letter table can come later.
3. **Tx 1:** transition `received → under_review`; if exactly one row changed, `move(external → pending_in, step review_start)` and history row. Zero rows → already done, carry on.
4. Wait a configurable delay (`REVIEW_DELAY_MS`), signalling in-progress to JetStream so the message isn't redelivered mid-review.
5. Decide: approve with probability `APPROVE_RATE`, otherwise reject.
6. **Tx 2:** transition `under_review → approved|rejected`; if exactly one row changed, `move()` with step `approve` or `reject` and history row. Zero rows → already decided (duplicate).
7. Update the delivery row's outcome: `applied` if Tx 2 changed state, otherwise `duplicate`; `failed` on an unexpected error (message left for redelivery).
8. Ack.

Crash behaviour:
- Crash between Tx 1 and Tx 2 → redelivered; Tx 1 is a no-op (already `under_review`, and `review_start` movement already exists), Tx 2 completes normally. `pending_in` is credited once.
- Crash before Tx 2 commits → redelivered, completes normally.
- Crash after Tx 2 commits, before ack → redelivered, recognised as a duplicate, acked, no balance change.

A delivery row left `in_progress` after a crash is expected; the redelivery gets its own row.

## Reconciliation

`scripts/reconcile.ts` checks, per account:

```text
pending_in == sum(amount) of payments in (under_review)
available  == sum(amount) of payments in (approved)

pending_in == net of balance_movements into/out of pending_in
available  == net of balance_movements into/out of available
```

Prints a per-account table and an overall difference; exit code non-zero if any difference ≠ 0. Also prints:
- payments stuck in `received` beyond a threshold (orphaned notifications — see *Known gaps*)
- deliveries vs applied: count of `event_deliveries` by outcome, and duplicate notifications from `notification_count`

## Configuration

`DATABASE_URL`, `NATS_URL`, `PORT`, `REVIEW_DELAY_MS`, `APPROVE_RATE`, `LOG_LEVEL`. Structured JSON logs including `request_id`, `payment_id`, `event_id`.

## Done when

- [ ] `docker compose up` starts Postgres, NATS, migrations, API and worker from a clean clone
- [ ] Same notification (same `network_transaction_id`) sent 5 times → one payment, `notification_count` = 5, balance changes once
- [ ] 10 concurrent notifications with the same `network_transaction_id` → one payment
- [ ] Same `network_transaction_id` with a different amount → `409`, no balance change
- [ ] Same `payment.received` event published twice with different NATS message IDs → applied once, two delivery rows (`applied` + `duplicate`)
- [ ] Same event processed by two concurrent consumers → applied once
- [ ] Worker crashes between Tx 1 and Tx 2, message redelivered → `pending_in` credited once, payment completes
- [ ] Calling `move()` twice with the same reference → second call is a no-op
- [ ] Worker killed mid-processing and restarted → every payment ends `approved` or `rejected`
- [ ] Reconciliation reports zero difference (against payments and against movements) after all of the above
- [ ] Integration tests cover the duplicate, concurrency, crash and `409` checks above
- [ ] README: how to run, what it demonstrates, known gaps
- (No tag for this slice: `v0.1-simple-ledger` is tagged once withdrawals are also in.)

## Known gaps (deliberate)

- **Commit-then-publish gap:** if the API commits but crashes before publishing, the payment stays `received` forever. Because `received` has no balance effect, this is an orphaned notification rather than stranded money. Fix in a later slice with a transactional outbox; until then, reconciliation lists payments stuck in `received` beyond a threshold.
- Approve/reject is random, not a real rules engine.
- No authentication on the API.

## Decisions

1. **Accounts:** demo accounts seeded by a migration; no `POST /accounts`.
2. **Tagging:** no tag for slice 1; `v0.1-simple-ledger` after withdrawals.
3. **`under_review`:** set by the worker when compliance picks the payment up, not by the API.
4. **Rejections:** modelled as returned to sender in the ledger; the network return itself is out of scope.
5. **IDs:** UUIDv7 stored as `uuid`, enforced by `CHECK (uuid_extract_version(…) = 7)`; app-generated, with `DEFAULT uuidv7()` on entity primary keys as a fallback only; TypeID-prefixed outside the database. Typeless references carry a `reference_type`. Primary keys: UUIDv7 for entities that may be referenced or exposed; `bigserial` for internal append-only logs. `balance_movements` counts as an entity because it becomes the double-entry transactions table.
6. **Incoming idempotency:** `UNIQUE (network, network_transaction_id)`; no client `Idempotency-Key` until withdrawals.
7. **API is ingestion only:** it never changes balances; all balance movements happen in the worker via the ledger module.
8. **No check-then-act:** duplicate and race protection comes from DB constraints and row counts, never from a prior `SELECT`.
9. **Queries:** raw parameterised SQL via `pg`, confined to the data-access layer, with explicit typed row mapping.
10. **Ledger is self-protecting:** `balance_movements` with `UNIQUE (reference_type, reference_id, reference_step)`; `move()` is a no-op on a duplicate reference. Payment state drives the workflow but is not the only thing preventing double movements, avoiding coupling the ledger to payment state.
11. **Duplicates are counted, not guarded, by a log:** `event_deliveries` (one row per delivery, no unique constraint) replaces `processed_events`; ingestion duplicates counted via `notification_count`.
12. **Money:** `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON; division via one helper with an explicit rounding mode. Rationale in `docs/decisions/0001-money-representation.md`.
