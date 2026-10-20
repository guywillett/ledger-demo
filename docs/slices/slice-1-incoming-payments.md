# Slice 1 — Incoming payments with pseudo approval

Status: agreed — ready to build
Implementation: Node.js / TypeScript (Go to follow in a later slice)

## Goal

The smallest end-to-end, runnable system that accepts incoming payments, holds them as pending, approves or rejects them asynchronously, and **provably changes balances exactly once**, however many times a request or event is repeated.

## In scope

- Repo skeleton and `docker compose up` for the whole system
- Postgres schema and migrations
- Payment API (create payment, read balance, read transaction history)
- Worker consuming events from NATS JetStream: books payments (posting) and runs the pseudo compliance review
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
│   └── events/payment.created.v1.json
├── db/migrations/
├── node/
│   ├── package.json
│   ├── src/
│   │   ├── api/          # Fastify app
│   │   ├── worker/       # consumer: wires compliance decision to ledger effects
│   │   ├── compliance/   # decides approve/reject — knows nothing about balances
│   │   ├── ingestion/    # records provider notifications; maps provider identifiers (IBAN) to our accounts
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
| iban | text | `UNIQUE`; how providers identify the account. Stored normalised (no spaces, upper case); format checked by a `CHECK`, mod-97 checksum checked in the app |
| provider_account_id | text null | `UNIQUE`; the provider's own ID for the account, when it has one |
| created_at | timestamptz | |

Demo accounts are created by a dedicated seed migration (zero balances, made-up but checksum-valid French IBANs). There is no account-creation endpoint in this slice.

**Provider identifiers are mapped at the edge.** A network or BaaS never knows our account IDs. Notifications identify the account credited by its IBAN (often a virtual IBAN per customer) and, with many providers, the provider's own account ID. Ingestion maps the IBAN to our `account_id`; everything downstream (events, worker, ledger) uses `account_id` only — the same edge-only pattern as TypeIDs.

Matching uses the IBAN, not the provider account ID: with vIBANs the IBAN is what identifies the customer, while the provider account ID may be absent, belong to a shared master account, or be an alias for the vIBAN, depending on the provider. The provider account ID is stored when present: it is the reference for the provider's API, statements and reconciliation, and for their support and compliance teams.

Slice 1 simplification: one IBAN and at most one provider account ID per account, single provider, no lifecycle. **Evolution path:** a `provider_accounts` table — the account as it exists at the provider:

| Column | Notes |
|---|---|
| id | UUIDv7 |
| account_id | FK to our `accounts` (see cardinality below) |
| provider | which BaaS issued it; provider IDs are only unique per provider |
| provider_account_id | nullable — not every provider has one (e.g. some vIBAN setups) |
| iban | the (v)IBAN, if any |
| status | `active`, `inactive`, `closed` — the lifecycle belongs to the provider account. Either side may change it (we request, the provider applies its own constraints). A closed account must stop matching new payments but still resolve historical ones |
| opened_at / closed_at | |

with `UNIQUE (provider, provider_account_id)` and `UNIQUE (provider, iban)`, plus child rows for vIBANs when one provider account carries many (e.g. one per end customer on a shared master account). A generic `account_identifiers (provider, scheme, value)` table is the more flexible alternative if providers vary a lot.

**Cardinality is a product decision**, not a technical one: several provider accounts feeding one internal account; each provider account being an internal account (customers having several accounts); or customers with several internal accounts, each fed by several provider accounts. A foreign key from `provider_accounts.account_id` to `accounts` supports the first and third; the second makes it one-to-one. Customers are a separate entity, out of scope here.

Transaction identifiers have the same problem in practice: a provider may expose several IDs for one transaction (network-level, provider-internal, general) across different endpoints and webhooks. Slice 1 has one `network_transaction_id`; a later `provider_transaction_refs (payment_id, provider, kind, value)` table would hold the rest, with the idempotency key chosen per provider.

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
| creditor_iban | text | the IBAN credited, as the provider sent it (normalised): the record of what we were told |
| provider_account_id | text null | the provider's account ID as sent, if any; stored for support and reconciliation, not used for matching |
| account_id | uuid FK | our mapping of `creditor_iban` |
| amount_minor | bigint | `CHECK (amount_minor > 0)` |
| currency | text | `CHECK (currency = 'EUR')` |
| network | text | `CHECK (network = 'SEPA')` |
| network_transaction_id | text | ID from the network / BaaS provider; `UNIQUE (network, network_transaction_id)` — the idempotency key for incoming payments |
| status | text | `created`, `received`, `under_review`, `approved`, `rejected`, `credited`, `returned` |
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
| reference_step | text | the step within it: `book`, `credit`, `return` |
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
created ──► received ──► under_review ──► approved ──► credited
                                     └──► rejected ──► returned
```

Every transition is a conditional update (`UPDATE … WHERE id = $1 AND status = $expected`). If zero rows change, the transition has already happened and the operation is a no-op. Payment state **drives the workflow**; the ledger protects itself independently via unique movement references (see *The `move()` function*). Two guards, each owned by its own module.

Balance effects:

| Transition | Movement | `reference_step` |
|---|---|---|
| (create) → created | none — notified, not yet booked | — |
| created → received | external → `pending_in` (funds booked) | `book` |
| received → under_review | none — compliance has picked it up | — |
| under_review → approved | none — the compliance decision only | — |
| under_review → rejected | none — the compliance decision only | — |
| approved → credited | `pending_in` → `available` | `credit` |
| rejected → returned | `pending_in` → external (returned to sender) | `return` |

What each state means:
- `created`: the provider's notification is recorded; nothing is booked yet.
- `received`: the funds are booked into `pending_in`. A posting / ledger concern.
- `under_review`: the compliance process has picked the payment up. A compliance concern; no money moves.
- `approved` / `rejected`: the compliance decision. No money moves yet.
- `credited`: the decision is posted; the funds are in `available`.
- `returned`: the decision is posted; the funds have left `pending_in` (returned to sender).

**Decisions are separate from their effects.** Each transition has exactly one owner, and the rule is simple: **compliance transitions never move money; posting transitions always do.**

| Transition | Owner |
|---|---|
| created → received | posting |
| received → under_review → approved \| rejected | compliance |
| approved → credited, rejected → returned | posting |

This is the seam for later splitting posting and compliance into separate consumers: neither ever changes a state the other owns, and the hand-off between them is the event for the state just entered (e.g. compliance publishes `payment.approved`; posting consumes it and produces `payment.credited`).

One status column shared by two owners works while the lifecycle is linear. If it grows (holds, partial returns, re-reviews), the next step is to split state by owner — e.g. `compliance_status` and `posting_status`, or a table per module — with the overall status derived from them. Out of scope for now.

`created` is set by the API; every later state by the worker. **The API never changes balances**; every balance change happens in the worker, through the ledger module.

**Event naming:** one event per state transition, named `payment.<new status>` and published when the payment enters that status (`payment.created`, `payment.received`, `payment.under_review`, `payment.approved`, `payment.rejected`, `payment.credited`, `payment.returned`). Status names are therefore part of the public event contract: renaming a state means a new event version. In slice 1 only `payment.created` is published; the others arrive when something consumes them.

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

Body: `{ network_transaction_id, creditor_iban, provider_account_id?, amount_minor, currency: "EUR", network: "SEPA" }`:
- `amount_minor` is a string of digits (e.g. `"1250"`), validated and converted to `BigInt`;
- `creditor_iban` is parsed into a branded `Iban` type (`type Iban = string & { readonly __brand: 'Iban' }`) by `parseIban`, which normalises (spaces removed, upper case) and then checks format, length and the mod-97 checksum. Only `parseIban` can produce an `Iban`, so any function taking one (e.g. the repository lookup) is guaranteed a normalised, valid IBAN ("parse, don't validate"). In the request schema it is a Zod `.transform()` calling `parseIban`;
- `provider_account_id` is optional, stored as received, and not used for matching or checked against `accounts.provider_account_id` (with vIBANs they can legitimately differ).

One statement (so no explicit transaction is needed): resolve `creditor_iban` to our account, insert the payment (`created`) and its first history row (a data-modifying CTE), or count a duplicate (`INSERT … SELECT … FROM accounts WHERE iban = $1 … ON CONFLICT …`, detailed below). No matching account → nothing inserted → `422`. **No balance change** — ingestion only.

Then publish `payment.created` to JetStream with `Nats-Msg-Id = event_id`.

Idempotency behaviour (keyed on `network` + `network_transaction_id`). "Same details" means `creditor_iban`, `amount_minor` and `currency` equal the stored row's, compared directly in SQL — every identifying detail is stored on the row, so no request hash is needed, and Node and Go share the exact rule by running the same SQL. `provider_account_id` is not compared: it is informational, and a provider that includes it in one retry and omits it in the next must not trigger a `409`; the first value received is kept.
- Same network ID, same details → `200` with the existing payment; `notification_count` incremented; no other effects
- Same network ID, different details → `409` and an `error` log naming the fields that differ (provider bug or suspicious — not a retry)
- Concurrent notifications with the same network ID → exactly one creates; the rest are duplicates or conflicts

All of this is decided by one statement: an `INSERT … SELECT … FROM accounts WHERE iban = $1 … ON CONFLICT (network, network_transaction_id) DO UPDATE SET notification_count = notification_count + 1 WHERE (creditor_iban, amount_minor, currency) = (EXCLUDED.…)`, returning whether a row was inserted (Postgres 18 `RETURNING old/new`). No row returned means either no matching account (`422`) or a conflict (`409`); a conflict takes priority when both apply. Which one is decided by reading the stored payment for the network ID after the statement (a fresh snapshot): one exists → `409` (and its details name the differing fields), none → `422`. This read only classifies a decision the statement has already made atomically. When the IBAN matches an account the result is exact even under concurrency, because `ON CONFLICT` waits for a competing insert and sees the committed row. When it matches none, a competing notification with the same network ID that has not yet committed when the read runs gives `422`: at that moment no payment with that ID is stored, so `422` is accurate.

A client-supplied `Idempotency-Key` header is not used here; it arrives with withdrawals, which are genuinely client-initiated.

Other responses: `400` for an invalid body (including an invalid IBAN), `422` for a valid IBAN that matches none of our accounts, `5xx` for unexpected failures. A refused notification whose stored payment differs in no compared field is an internal inconsistency (the SQL and TypeScript duplicate rules have drifted, or an account's IBAN changed), not a conflict: `500` and an `error` log.

Errors are RFC 9457 problem details (`application/problem+json`): a `400` lists each problem in `errors` (path and message, never the input values), and a `409` lists the names of the differing fields in `differing_fields` (never their stored values). Unknown body fields are ignored rather than rejected (tolerant reader), and are neither stored nor logged. The contract is `contracts/openapi.yaml`, written by hand and shared with the Go implementation.

Response `201`: the payment, including status.

#### Webhook semantics (demo vs. real provider)

This endpoint uses conventional API status codes because they make tests and curl examples self-explanatory. A real network / BaaS webhook works differently: senders typically retry on any non-2xx and ignore the response body. The real contract would be **2xx for anything recorded or deliberately handled; 5xx only when a retry could help**. The exact codes depend on the provider's contract (some treat 4xx as permanent and do not retry); the handler carries a comment pointing here.

| Case | Demo | Real webhook |
|---|---|---|
| New notification | `201` | 2xx |
| Duplicate, same details | `200` | 2xx — this is the normal retry case |
| Transient failure (e.g. database down) | `5xx` | `5xx` — the provider retries; idempotency makes that safe |
| Same network ID, different details | `409` | 2xx, record and alert — retrying cannot fix it |
| Invalid body | `400` | 2xx (or provider-specific 4xx), record and alert — an integration bug, not something a retry fixes |
| Account not managed by this app (e.g. treasury or other system accounts — a BaaS may send events for every account it holds) | `422` | 2xx and ignore, logged at `info` — out of scope for this app |
| Account that should be ours but is unknown | `422` | 2xx, record as unmatched (suspense) and alert — the money has already arrived at the bank and cannot be refused over HTTP; it is matched or returned to sender by a separate process |

Telling "not ours" from "unknown" needs a rule from the provider's data (e.g. an account-type field, the provider's account ID, or a configured list of ignored IBANs). In this slice every unmatched IBAN gets `422`. Recording unmatched payments in suspense would make `incoming_payments.account_id` nullable.

#### Logging

Fastify is given our pino logger (`loggerInstance`), so every request gets an access log line (method, URL, status, response time) and a `request_id`. Domain log lines on top of that:

| Event | Level |
|---|---|
| Payment created | `info` |
| Duplicate notification, same details | `info`, with `duplicate: true` and the new `notification_count` (provider retries are normal, so not a warning) |
| Invalid body, or IBAN matching none of our accounts | `warn` |
| Same network ID, different details | `error` (integrity problem: someone should look) |
| Unexpected failure (`5xx`) | `error` |

Log IDs and amounts, not the request body (later fields such as payer name are personal data). Log `creditor_iban` masked (e.g. `FR76…0193`). Bind `request_id` and `payment_id` (strings) on child loggers; put `amount_minor` in the log call object, where BigInts are converted to strings.

### `GET /accounts/:id/balance`
`{ pending_in, available, currency }`, with balances as digit strings (e.g. `{ "pending_in": "1250", "available": "0", "currency": "EUR" }`).

### `GET /accounts/:id/transactions`
Payments, newest first, each with current status. Optional `?include=history` adds the status-history rows.

## Event: `payment.created` v1

```json
{
  "event_id": "evt_…",
  "event_type": "payment.created",
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

`event_id` identifies the business event; the NATS message ID is transport only. The event carries our `account_id`, not the IBAN: provider identifiers stop at the ingestion edge. The JSON schema lives in `contracts/events/`.

## Worker (posting and compliance)

Durable JetStream consumer with explicit acks. For each message:

1. **Log the delivery** in its own small transaction: insert into `event_deliveries` with outcome `in_progress`.
2. Validate with Zod; invalid → outcome `invalid`, log and terminate the message (no infinite redelivery). A dead-letter table can come later.
3. **Tx 1 (book):** transition `created → received`; if exactly one row changed, `move(external → pending_in, step book)` and history row. Zero rows → already done, carry on.
4. **Tx 2 (start review):** transition `received → under_review` and history row; no movement. Zero rows → already done, carry on.
5. Wait a configurable delay (`REVIEW_DELAY_MS`), signalling in-progress to JetStream so the message isn't redelivered mid-review.
6. Decide: approve with probability `APPROVE_RATE`, otherwise reject.
7. **Tx 3 (decide):** transition `under_review → approved|rejected` and history row; no movement. Zero rows → already decided: the recorded decision stands and the one just made is discarded (a redelivery must never re-decide).
8. **Tx 4 (post the decision):** transition `approved → credited` with `move(pending_in → available, step credit)`, or `rejected → returned` with `move(pending_in → external, step return)`, and history row. Which one applies comes from the payment's recorded state (try both conditional updates; at most one matches), not from step 6. Zero rows → already posted.
9. Update the delivery row's outcome: `applied` if any of Tx 1–4 changed state, otherwise `duplicate`; `failed` on an unexpected error (message left for redelivery).
10. Ack.

Tx 1–4 are separate transactions on purpose: they alternate between owners (posting, compliance, compliance, posting) and become different consumers when those are split.

Crash behaviour:
- Crash after any of Tx 1–3 → redelivered; completed transactions are no-ops (state already moved on; movements already exist), the rest completes. `pending_in` is credited once.
- Crash after Tx 3, before Tx 4 → redelivered; the decision is not re-made, Tx 4 posts the recorded one.
- Crash after Tx 4 commits, before ack → redelivered, every transaction is a no-op, recorded as a duplicate, acked, no balance change.

A delivery row left `in_progress` after a crash is expected; the redelivery gets its own row.

## Reconciliation

`scripts/reconcile.ts` checks, per account:

```text
pending_in == sum(amount) of payments in (received, under_review, approved, rejected)
available  == sum(amount) of payments in (credited)

pending_in == net of balance_movements into/out of pending_in
available  == net of balance_movements into/out of available
```

Prints a per-account table and an overall difference; exit code non-zero if any difference ≠ 0. Also prints:
- payments stuck in `created` beyond a threshold: received by the bank but never booked (see the commit-then-publish gap under *Known gaps*)
- deliveries vs applied: count of `event_deliveries` by outcome, and duplicate notifications from `notification_count`

## Configuration

`DATABASE_URL`, `NATS_URL`, `PORT`, `REVIEW_DELAY_MS`, `APPROVE_RATE`, `LOG_LEVEL`. Structured JSON logs including `request_id`, `payment_id`, `event_id`.

## Done when

- [ ] `docker compose up` starts Postgres, NATS, migrations, API and worker from a clean clone
- [ ] Same notification (same `network_transaction_id`) sent 5 times → one payment, `notification_count` = 5, balance changes once
- [ ] 10 concurrent notifications with the same `network_transaction_id` → one payment
- [ ] Same `network_transaction_id` with a different amount or `creditor_iban` → `409`, nothing changed
- [ ] Notification for a valid IBAN that matches none of our accounts → `422`, nothing stored; invalid IBAN (bad checksum) → `400`
- [ ] Same `payment.created` event published twice with different NATS message IDs → applied once, two delivery rows (`applied` + `duplicate`)
- [ ] Same event processed by two concurrent consumers → applied once
- [ ] Worker crashes after booking (Tx 1) and before the decision (Tx 3), message redelivered → `pending_in` credited once, payment completes
- [ ] Worker crashes after the decision (Tx 3) and before posting it (Tx 4), message redelivered → the decision is not re-made; the recorded one is posted once
- [ ] Calling `move()` twice with the same reference → second call is a no-op
- [ ] Worker killed mid-processing and restarted → every payment ends `credited` or `returned`
- [ ] Reconciliation reports zero difference (against payments and against movements) after all of the above
- [ ] Integration tests cover the duplicate, concurrency, crash, `409` and unmatched-IBAN checks above
- [ ] README: how to run, what it demonstrates, known gaps
- (No tag for this slice: `v0.1-simple-ledger` is tagged once withdrawals are also in.)

## Known gaps (deliberate)

- **Commit-then-publish gap:** if the API commits the payment but crashes before publishing `payment.created`, nothing ever processes it and it stays `created` forever. The money has arrived at the bank but is never booked, so the customer never gets it: stranded money. Our ledger stays internally consistent (no balance without a booked payment), which is exactly why internal reconciliation cannot see the problem; only the "stuck in `created`" check below, or reconciling against the provider's own statement, catches it. This gap has been present since the API first published events; it is fixed in a later slice with a transactional outbox.
- Approve/reject is random, not a real rules engine.
- No authentication on the API. In particular, `POST /payments` does not verify the sender: real webhooks are authenticated, typically by an HMAC signature header checked against a shared secret.
- `POST /payments` uses demo status codes rather than a real webhook contract (see *Webhook semantics*), and does not distinguish accounts outside this app from genuinely unknown ones.
- One IBAN and at most one provider account ID per account, single provider, no lifecycle; see `provider_accounts` under *Data model* for the evolution path.

## Decisions

1. **Accounts:** demo accounts seeded by a migration (with test IBANs); no `POST /accounts`.
2. **Tagging:** no tag for slice 1; `v0.1-simple-ledger` after withdrawals.
3. **States:** `created` (set by the API: notified, nothing booked) → `received` (funds booked into `pending_in`) → `under_review` (compliance has picked it up) → `approved | rejected` (the decision) → `credited | returned` (the decision posted). Decisions are separate from their effects: compliance transitions never move money, posting transitions always do; each transition has one owner.
4. **Rejections:** modelled as returned to sender in the ledger; the network return itself is out of scope.
5. **IDs:** UUIDv7 stored as `uuid`, enforced by `CHECK (uuid_extract_version(…) = 7)`; app-generated, with `DEFAULT uuidv7()` on entity primary keys as a fallback only; TypeID-prefixed outside the database. Typeless references carry a `reference_type`. Primary keys: UUIDv7 for entities that may be referenced or exposed; `bigserial` for internal append-only logs. `balance_movements` counts as an entity because it becomes the double-entry transactions table.
6. **Incoming idempotency:** `UNIQUE (network, network_transaction_id)`; no client `Idempotency-Key` until withdrawals.
7. **API is ingestion only:** it never changes balances; all balance movements happen in the worker via the ledger module.
8. **No check-then-act:** duplicate and race protection comes from DB constraints and row counts, never from a prior `SELECT`.
9. **Queries:** raw parameterised SQL via `pg`, confined to the data-access layer, with explicit typed row mapping.
10. **Ledger is self-protecting:** `balance_movements` with `UNIQUE (reference_type, reference_id, reference_step)`; `move()` is a no-op on a duplicate reference. Payment state drives the workflow but is not the only thing preventing double movements, avoiding coupling the ledger to payment state.
11. **Duplicates are counted, not guarded, by a log:** `event_deliveries` (one row per delivery, no unique constraint) replaces `processed_events`; ingestion duplicates counted via `notification_count`.
12. **Money:** `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON; division via one helper with an explicit rounding mode. Rationale in `docs/decisions/0001-money-representation.md`.
13. **`POST /payments` status codes:** conventional API codes for the demo; the real webhook contract (2xx for anything recorded or handled, 5xx only when a retry helps, accounts outside this app acknowledged and ignored) is documented under *Webhook semantics*.
14. **Provider identifiers mapped at the edge:** notifications identify accounts by `creditor_iban`, mapped to `account_id` during ingestion; the IBAN as received is stored on the payment; nothing downstream sees provider identifiers. The provider account ID is optional, stored when sent, and not used for matching or for duplicate detection. One IBAN and at most one provider account ID per account for now; `provider_accounts` (per provider, with lifecycle status) is the evolution path.
15. **Event naming:** `payment.<new status>`, one event per state transition, published on entering that status; status names are part of the event contract. Only `payment.created` is published in slice 1.
16. **No request hash:** duplicate detection compares the stored `creditor_iban`, `amount_minor` and `currency` directly in the upsert. Every identifying detail is stored on the row, so a hash would add a cross-language format to keep in sync and hide which field differed.
