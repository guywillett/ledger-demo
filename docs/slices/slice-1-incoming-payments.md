# Slice 1 — Incoming payments with pseudo approval

Since ADR 0002, payments are called deposits (and the worker the deposit-worker); this spec keeps the names it was written with.

Status: done (Node.js / TypeScript); every item under *Done when* is met
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
├── scripts/demo.sh       # language-neutral demo: HTTP and compose only
├── nats/
│   ├── streams/          # JetStream stream definitions
│   ├── consumers/        # durable consumer definitions, per stream
│   └── setup.sh          # applies them (the nats-setup compose service)
├── node/
│   ├── package.json
│   ├── src/
│   │   ├── api/            # Fastify app
│   │   ├── worker/         # consumer: wires compliance decision to ledger effects
│   │   ├── payments/       # the payment entity: lifecycle, status transitions, reads
│   │   ├── ingestion/      # the provider-facing edge: records notifications, maps provider identifiers (IBAN) to our accounts, announces new payments
│   │   ├── compliance/     # decides approve/reject and owns the review transitions — knows nothing about balances
│   │   ├── ledger/         # posting transitions + move() — the only code that touches balances
│   │   ├── reconciliation/ # checks that balances, movements and payment statuses agree
│   │   └── shared/         # infrastructure (config, db, nats, logging) and shared value types (ids, iban, event contracts)
│   ├── scripts/reconcile.ts
│   └── test/
└── docs/
    ├── slices/slice-1-incoming-payments.md
    └── decisions/
```

## Data model

Amounts are **integer minor units** (cents): `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON. No floats anywhere. See `docs/decisions/0001-money-representation.md`.

**IDs:** entity primary keys and event IDs are native `uuid` columns holding UUIDv7 (time-ordered, generated in the app). Every UUID column has `CHECK (uuid_extract_version(col) = 7)`; entity primary keys also have `DEFAULT uuidv7()` as a fallback for manual inserts only — the app always supplies IDs. Event IDs and references have no default. **Primary key rule:** UUIDv7 for entities that may be referenced or exposed (accounts, payments, balance movements); `bigserial` for internal append-only logs that are never referenced or exposed (`payment_status_history`, `event_deliveries`). Outside the database — API, events, logs — they appear as TypeIDs with a type prefix (`acct_…`, `pay_…`, `evt_…`, `mov_…`). Conversion happens only in the data-access layer and the request/event schemas. Never store IDs as text or cast them in queries.

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
One row per account, created at zero with the account by an `AFTER INSERT` trigger on `accounts` (so it holds however the account is inserted: seed migration, tests, `psql`). `move()` updates the row but never creates it, and `GET /accounts/:id/balance` treats a missing row as a missing account. Nothing stops the row being deleted later; reconciliation reports any account without one.
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
**Append-only**, enforced by triggers like `balance_movements`.

| Column | Type | Notes |
|---|---|---|
| id | bigserial PK | |
| payment_id | uuid FK | |
| from_status | text null | null for the first row |
| to_status | text | |
| reason | text null | e.g. rejection reason |
| at | timestamptz | |

### `balance_movements`
One row per balance movement, written only by `move()`. The ledger's own idempotency guard and audit trail; grows into the double-entry transactions table later. **Append-only**, enforced by triggers that reject `UPDATE`, `DELETE` and `TRUNCATE`. The triggers suit the demo, where everything connects as one database user that owns the tables; in production the primary control would be database roles (each service its own role with only the privileges it needs, e.g. `INSERT` and `SELECT` on the audit tables; migrations as a separate owner role), with triggers as a backstop. See *Known gaps*.
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
| event_id | uuid null | business event ID (TypeID prefix `evt`); indexed, not unique. Null only for a message too broken to read one: the row is written before validation, so `CHECK (event_id IS NOT NULL OR outcome IN ('in_progress', 'invalid'))` |
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
2. If no row was inserted → read the existing movement: if its account, buckets or amount differ, throw (the reference is being reused for something else: a bug, not a retry); otherwise return `'duplicate'`; balances untouched.
3. Otherwise update the `account_balances` columns for the non-external side(s) in one fixed statement (`SET pending_in = pending_in + $2, available = available + $3`, the changes computed per bucket) and return `'applied'`. The `CHECK (… >= 0)` constraints reject an overdraft, reported as an `OverdraftError` naming the account and bucket (the database error kept as its `cause`), and a missing balance row is an error.

The transitions are split by owner, each a conditional status update plus its history row in the caller's transaction: `ledger/posting.ts` has `book`, `credit`, `returnToSender` (each also calls `move()`) and `postDecision` (tries `credit`, then `returnToSender`); `compliance/transitions.ts` has `startReview` and `decide`. Each returns `'applied'` or `'skipped'` (already done, or the payment is in another state). The legal transitions are a TypeScript union grouped by owner (`PostingTransition`, `ComplianceTransition` in `payments/status.ts`), and `transitionStatus` accepts only those: an illegal pair such as `credited → created` does not compile, and a `@ts-expect-error` test proves it.

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

Then publish `payment.created` to JetStream with `Nats-Msg-Id = <payment_id>.created` (one message ID per payment and event type; see *Stream `PAYMENTS`*), and wait for the stream's acknowledgement (2 s timeout). If the publish fails (NATS down, timeout), the payment stays `created` and the response is `503` with `Retry-After`: the provider's retry arrives as a duplicate, and a duplicate of a payment still `created` publishes `payment.created` again (see below). That retry is what makes the publish eventually happen; see *Known gaps* for what remains.

Idempotency behaviour (keyed on `network` + `network_transaction_id`). "Same details" means `creditor_iban`, `amount_minor` and `currency` equal the stored row's, compared directly in SQL — every identifying detail is stored on the row, so no request hash is needed, and Node and Go share the exact rule by running the same SQL. `provider_account_id` is not compared: it is informational, and a provider that includes it in one retry and omits it in the next must not trigger a `409`; the first value received is kept.
- Same network ID, same details → `200` with the existing payment; `notification_count` incremented. If the payment is still `created`, `payment.created` is published again (an earlier publish may have failed); once the worker has moved it on, a duplicate has no other effect. A re-publish carries a new `event_id` (event IDs aren't stored without an outbox) but the same message ID, so within the stream's duplicate window the stream drops it (the original event stays, with its original `event_id`). Beyond the window consumers may see two events for one payment; they are idempotent on the payment's state, and the delivery log counts the second as a duplicate.
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
`{ pending_in, available, currency }`, with balances as digit strings (e.g. `{ "pending_in": "1250", "available": "0", "currency": "EUR" }`). Read by `ledger/balances.ts`. `400` if `:id` isn't an `acct_` ID, `404` if there is no such account.

### `GET /accounts/:id/transactions`
Payments, newest first, each with current status, in the same shape as the `POST /payments` response: `{ "payments": [...], "next_before": "pay_…" | null }`. Optional `?include=history` adds each payment's status-history rows (`from_status`, `to_status`, `reason`, `at`), fetched for the whole page in one query.

**Pagination** seeks on the payment ID: UUIDv7 IDs sort by creation time, so the ID is both the order and the cursor. `?limit` (default 50, at most 200) and `?before=<next_before from the previous page>`; the query is `WHERE account_id = $1 AND id < $before ORDER BY id DESC`, served by the `(account_id, id DESC)` index. No offsets, and a page doesn't shift when new payments arrive. `400` for an invalid ID or query parameter, `404` for no such account, an empty page for an account without payments; unknown query parameters are ignored.

The list is for **display, not sync**. IDs are generated before the insert, so ID order is creation order to the millisecond, not commit order: a payment can commit after another with a larger ID, and a client that keeps a checkpoint ("everything after the last ID I saw") could miss it for good. The list also doesn't surface later status changes to a client that has paged past a payment. Pages are still correct (IDs are unique and totally ordered, so nothing repeats or is skipped among rows visible at read time). A client keeping its own copy should consume events (`payment.<status>`), not poll this list; commit-ordered alternatives are a polling safety lag with idempotent re-reads, CDC from the write-ahead log, or a sequencing outbox relay.

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

`event_id` identifies the business event; the NATS message ID is transport only. The event carries our `account_id`, not the IBAN: provider identifiers stop at the ingestion edge. The JSON schema lives in `contracts/events/`. The subject is the event type (`payment.created`).

### Stream `PAYMENTS`

Defined in `nats/streams/PAYMENTS.json` and applied by the `nats-setup` compose service (`nats/setup.sh`) on every `docker compose up`, like the migrations: one language-neutral definition for Node and Go. The apps don't create streams; they check at startup that it exists.

| Setting | Value | Why |
|---|---|---|
| `subjects` | `payment.>` | every `payment.<status>` event, present and future, in one ordered log |
| `storage` | `file` | survives a NATS restart (the `natsdata` volume) |
| `num_replicas` | `1` | single server; production would use 3 |
| `retention` | `limits` | messages stay until they age out, whether consumed or not: replay and inspection, and several consumer types (posting, compliance) can read the same events. `workqueue` would allow one consumer per subject |
| `max_age` | 7 days | the limit; sizes unlimited at demo volume; `discard: old`. With `limits` retention messages age out whether or not they were consumed: if the worker were down for more than 7 days, unprocessed `payment.created` events would vanish and those payments stay `created` (stranded money; the "stuck in `created`" reconciliation check catches it) |
| `duplicate_window` | 2 minutes (default) | a repeated `Nats-Msg-Id` within the window is acknowledged (`duplicate: true`) but not stored again. The message ID is `<payment_id>.created`, not `event_id`, so this absorbs repeats of the same announcement: a publish that timed out but was stored and then retried, or provider retries while the worker is slow. The likeliest real case: while the client is reconnecting it buffers outgoing publishes, so a publish can time out (`503`) and still reach the stream once the connection returns; the provider's retry then re-announces it, and the per-payment message ID is what makes the stream drop that second copy (if it lands within the window). Noise reduction only: exactly-once effects come from the database guards |

## Worker (posting and compliance)

Durable JetStream consumer with explicit acks, defined in `nats/consumers/PAYMENTS/worker.json` and applied by `nats-setup` like the stream. Every worker instance reads from this one consumer; JetStream gives each message to one of them, so instances scale horizontally. Each instance handles up to `WORKER_CONCURRENCY` messages at once (safe: every step is a per-payment conditional transition and `move()` guards itself).

**Pulling:** the worker fetches exactly as many messages as it has free slots, and none while every slot is busy, so every message it holds is being handled (and signalling in-progress) from the moment it arrives. A prefetching `consume()` would hold extra messages in a client buffer with their `ack_wait` running and no one signalling; with reviews longer than `ack_wait` they were redelivered and processed twice (an e2e test covers this). An idle fetch long-polls for 5 s on the server (the client adds an idle heartbeat of half that, so a dead connection is noticed meanwhile), then the worker fetches again. A failed fetch is retried after 1 s, doubling to 30 s: logged at `warn` when transient (e.g. NATS reconnecting); a missing consumer or stream is a configuration problem, logged at `error` on each attempt, and after 5 attempts the worker shuts down with a failing exit code, so the restart policy and the logs make it obvious. On shutdown a waiting fetch is closed at once. Under a steady backlog every freed slot is refilled at once (each fetch is filled immediately, and the loop fetches again as soon as a handler finishes); but a burst arriving while a mostly satisfied fetch is still open can wait up to `expires` (5 s) for the next fetch, since only the open fetch's remaining slots are requested meanwhile. A shorter `expires` narrows that, at the cost of more pull requests while idle; worth measuring in the load-testing slice.

| Consumer setting | Value | Why |
|---|---|---|
| `durable_name` | `worker` | one consumer shared by all instances |
| `filter_subject` | `payment.created` | the only event the worker handles in slice 1 |
| `ack_policy` | `explicit` | acked individually, after the outcome is recorded |
| `deliver_policy` | `all` | on first start, process everything already in the stream |
| `ack_wait` | 10 s | short, because long work signals in-progress every 3 s; a crashed worker's message is redelivered within ~10 s |
| `max_deliver` | 100 | large: invariant violations are terminated by the handler itself, so the limit only bounds transient failures (with the handler's growing retry delay, roughly 1.5 hours) |
| `max_ack_pending` | 1000 | bound on unacked messages across all instances |

No consumer `backoff`: on JetStream it also replaces `ack_wait` for messages still being processed (the first step becomes the ack timeout). Instead the handler sets the delay on each `nak`: 1 s, doubling per attempt, at most 60 s.

For each message:

1. **Log the delivery** in its own small transaction: insert into `event_deliveries` with outcome `in_progress`.
2. Validate with Zod; invalid → outcome `invalid`, log and terminate the message (no infinite redelivery). A dead-letter table can come later. Parsing turns the TypeIDs into our UUIDs (checking each holds a UUIDv7), the amount into a `BigInt` and `occurred_at` into a `Date`: the schema's input type is the wire format producers build, its output type what consumers work with.
3. **Tx 1 (book):** transition `created → received`; if exactly one row changed, `move(external → pending_in, step book)` and history row. Zero rows → already done, carry on.
4. **Tx 2 (start review):** transition `received → under_review` and history row; no movement. Zero rows → already done, carry on.
5. If this delivery started the review (Tx 2 applied), wait a configurable delay (`REVIEW_DELAY_MS`), signalling in-progress to JetStream so the message isn't redelivered mid-review. Duplicates, unknown payments and redeliveries after a crash between Tx 2 and Tx 3 don't wait: the review has already happened from the system's point of view, and Tx 3 still records at most one decision.
6. Decide: approve with probability `APPROVE_RATE`, otherwise reject.
7. **Tx 3 (decide):** transition `under_review → approved|rejected` and history row; no movement. Zero rows → already decided: the recorded decision stands and the one just made is discarded (a redelivery must never re-decide).
8. **Tx 4 (post the decision):** transition `approved → credited` with `move(pending_in → available, step credit)`, or `rejected → returned` with `move(pending_in → external, step return)`, and history row. Which one applies comes from the payment's recorded state (try both conditional updates; at most one matches), not from step 6. Zero rows → already posted.
9. Update the delivery row's outcome: `applied` if any of Tx 1–4 changed state, otherwise `duplicate`; `failed` on an unexpected error. A transient failure (e.g. the database is unavailable) `nak`s the message with a delay, so it is redelivered. An invariant violation (`LedgerInconsistencyError`, `MovementConflictError`, `OverdraftError`, or any other `CHECK` violation) is a bug that retrying can't fix: it is logged at `fatal` with `alert: true` and the message is terminated, leaving the payment where it stopped for someone to investigate. If nothing changed because the payment doesn't exist (e.g. after a database reset), the outcome is `invalid` and the message is terminated; that read only classifies the outcome, after every transition has been attempted.
10. Ack.

Tx 1–4 are separate transactions on purpose: they alternate between owners (posting, compliance, compliance, posting) and become different consumers when those are split.

Crash behaviour:
- Crash after any of Tx 1–3 → redelivered; completed transactions are no-ops (state already moved on; movements already exist), the rest completes. `pending_in` is credited once.
- Crash after Tx 3, before Tx 4 → redelivered; the decision is not re-made, Tx 4 posts the recorded one.
- Crash after Tx 4 commits, before ack → redelivered, every transaction is a no-op, recorded as a duplicate, acked, no balance change.

A delivery row left `in_progress` after a crash is expected; the redelivery gets its own row.

Because each of Tx 1–4 commits on its own, "crashed after Tx N" is exactly the database state of Tx 1…N committed plus a delivery row left `in_progress`. The handler tests create that state directly and run the handler as the redelivery, which covers the crash scenarios deterministically without test hooks in the production code; a process-level test also kills a real worker mid-review.

## Reconciliation

The ledger keeps three views of the same money: **balances** (`account_balances`, the current figures, a cache only `move()` changes), **movements** (`balance_movements`, the append-only journal) and **payments** (what each status says should be booked). Reconciliation checks that they agree. `node/scripts/reconcile.ts` (`npm run reconcile`, or the on-demand compose service: `docker compose run --rm reconcile`) runs every query in one `REPEATABLE READ READ ONLY` transaction, so it sees one consistent snapshot while the worker carries on (each worker step commits its status, movement and balance change together). The checks live in `src/reconciliation/reconcile.ts`.

Per account:

```text
pending_in == sum(amount) of payments in (received, under_review, approved, rejected)
available  == sum(amount) of payments in (credited)

pending_in == net of balance_movements into/out of pending_in
available  == net of balance_movements into/out of available
```

An account without a balance row is reported as a difference too (the checks start from `accounts`, not `account_balances`).

Per payment, its movements must match its status, because sums can hide two errors that cancel out: `created` none; `received`, `under_review`, `approved`, `rejected` exactly `book`; `credited` `book` and `credit`; `returned` `book` and `return`; all on the payment's own account and for the payment's amount. Movements that reference no payment are reported too.

Also reported:
- payments stuck in a non-final status (`created`, `received`, `under_review`, `approved`, `rejected`) beyond a threshold since they entered it, listed with their status and age. The balance checks can't see these: a stuck payment's funds are fully accounted for (in `pending_in`, or not booked at all), the payment just never finishes. The status tells the cause apart:
  - `created`: received by the bank but never booked, e.g. the commit-then-publish gap (see *Known gaps*);
  - any later non-final status: processing stopped half-way, e.g. a message that exhausted `max_deliver` or was terminated by the worker (see the dead-letter gap under *Known gaps*), with the funds held in `pending_in`.
- deliveries: `event_deliveries` by outcome (all accounts), `in_progress` rows older than the threshold (the trace a crash leaves), and duplicate notifications (`notification_count - 1`, summed).

Output is a table of the accounts and payments that disagree plus a summary, or `--json`. Options: `--account acct_…` (repeatable) to check only some accounts, `--stuck-after <minutes>` (default 10), `--fail-on-stuck`. Exit codes: `0` the books agree; `1` they disagree (any difference or mismatch); `2` they agree but payments are stuck and `--fail-on-stuck` was given; `3` it couldn't run. Stuck payments alone don't make the books wrong, so they don't fail it by default.

In production this would run on a schedule (alerting on a non-zero exit), after incidents, deploys and data migrations, and before a financial close. Comparing our books with the provider's statement (external reconciliation) is the other half, out of scope here. The test database holds deliberate inconsistencies left by tests (e.g. movements for no payment, corrupted balances), so a whole-database run there reports differences by design; the e2e tests reconcile the accounts they used instead.

**At scale** it would need reworking (noted in the code): the per-payment check reads every payment into memory (node-postgres buffers a whole result), so it should compare in SQL and return only mismatches, or stream through a cursor; and every query scans whole tables inside one long snapshot, which stops `VACUUM` cleaning up behind it. Run it on a read replica, in batches of accounts, or incrementally from a checkpoint.

## Configuration

`DATABASE_URL`, `NATS_URL`, `PORT` (API), `REVIEW_DELAY_MS`, `APPROVE_RATE`, `WORKER_CONCURRENCY` (worker), `LOG_LEVEL`. Structured JSON logs including `request_id`, `payment_id`, `event_id`.

## Done when

- [x] `docker compose up` starts Postgres, NATS, migrations, API and worker from a clean clone: checked with a fresh `git clone` as its own compose project (`up -d --build`, then `scripts/demo.sh`, `npm ci`, `npm test`, `npm run test:e2e`)
- [x] Same notification (same `network_transaction_id`) sent 5 times → one payment, `notification_count` = 5, balance changes once: `test/api/payments.test.ts` ("records the same notification sent 5 times once"); `scripts/demo.sh` step 1
- [x] 10 concurrent notifications with the same `network_transaction_id` → one payment: `test/api/payments.test.ts` ("records 10 concurrent identical notifications once"); `scripts/demo.sh` step 2
- [x] Same `network_transaction_id` with a different amount or `creditor_iban` → `409`, nothing changed: `test/api/payments.test.ts` ("rejects a different amount with 409 and changes nothing", "rejects a different IBAN of ours with 409"); `scripts/demo.sh` step 3 *(Since slice 2 step 6b: `204`, with the conflict recorded in `payment_notification_conflicts`; `test/api/incoming-payments.test.ts`.)*
- [x] Notification for a valid IBAN that matches none of our accounts → `422`, nothing stored; invalid IBAN (bad checksum) → `400`: `test/api/payments.test.ts` ("answers 422 for a valid IBAN that is not ours, storing nothing", "answers 400 for a bad IBAN checksum"); `scripts/demo.sh` step 4 *(Since slice 2 step 6b: `204`, with the payment recorded in suspense (status `unmatched`); an invalid IBAN is still `400`.)*
- [x] Same `payment.created` event published twice with different NATS message IDs → applied once, two delivery rows (`applied` + `duplicate`): `test/worker/handler.test.ts` ("applies the same event once when delivered twice with different message IDs")
- [x] Same event processed by two concurrent consumers → applied once: `test/worker/handler.test.ts` ("applies the same event once when two consumers process it concurrently"); `test/e2e/worker.e2e.ts` (two workers sharing the consumer)
- [x] Worker crashes after booking (Tx 1) and before the decision (Tx 3), message redelivered → `pending_in` credited once, payment completes: `test/worker/handler.test.ts` ("completes after a crash between booking and the decision")
- [x] Worker crashes after the decision (Tx 3) and before posting it (Tx 4), message redelivered → the decision is not re-made; the recorded one is posted once: `test/worker/handler.test.ts` ("posts the recorded decision after a crash between deciding and posting, never re-deciding")
- [x] Calling `move()` twice with the same reference → second call is a no-op: `test/ledger/move.test.ts` ("is a no-op when called twice with the same reference", and concurrently)
- [x] Worker killed mid-processing and restarted → every payment ends `credited` or `returned`: `test/e2e/worker.e2e.ts` ("completes a payment once after the worker is killed mid-review and restarted"); `scripts/demo.sh` step 5
- [x] Reconciliation reports zero difference (against payments and against movements) after all of the above: `test/e2e/worker.e2e.ts` ("leaves the books consistent for every account it used"); `scripts/demo.sh` (reconciles the whole database, nothing stuck), on a fresh clone and on the development database
- [x] Integration tests cover the duplicate, concurrency, crash, `409` and unmatched-IBAN checks above: as listed (`npm test`, `npm run test:e2e`) *(Since slice 2 step 6b the conflict and unmatched cases answer `204`, the effect recorded.)*
- [x] README: how to run, what it demonstrates, known gaps
- (No tag for this slice: `v0.1-simple-ledger` is tagged once withdrawals are also in.)

## Scaling notes: hot accounts

Every movement updates its account's single `account_balances` row, and Postgres holds that row lock until the transaction commits. Movements for **one** account therefore run strictly one after another: per-account throughput is capped at roughly 1 / (transaction duration), e.g. about 200 movements a second at 5 ms per posting transaction, however many workers run. Different accounts don't contend. It matters only for individually busy accounts: a large merchant, an omnibus or treasury account, and above all **system accounts**.

**System accounts are the trap for double-entry.** Once every booking also debits a system account such as `external_incoming`, that one account takes part in every transaction and serialises the whole system. Today `external` has no balance row (its side of `move()` is a no-op); keep that property: no cached balance row for system accounts, their balances derived from entries when needed.

Techniques, roughly cheapest first:

1. **Short transactions.** Lock the balance row as late as possible; no network calls inside the transaction. (Already so: status update, then movement, then commit.)
2. **Credit/debit asymmetry.** A credit can never fail an overdraft check, so it needs no lock at decision time: an append-only entry, with the balance updated later. Only debits must be serialised, because "is there enough?" must be answered atomically.
3. **Snapshot plus entries.** Balance = last snapshot + sum of entries since; entries are append-only inserts (no contention), a background job rolls up snapshots. Reads cost a little more.
4. **Sharded sub-balances.** N balance rows per account, each movement picking one; reads sum them. Credits scale; debits must reserve from one shard or lock all.
5. **A purpose-built ledger database** (e.g. TigerBeetle), which batches transfers and avoids per-row locks.

No change in slice 1. The load-testing slice should measure it: throughput against many accounts (scales with workers) versus one account (flattens at the per-row limit), then apply one technique, e.g. 3, and show the difference.

## Known gaps (deliberate)

- **Commit-then-publish gap:** if the API commits the payment but crashes before publishing `payment.created`, nothing ever processes it and it stays `created` forever. The money has arrived at the bank but is never booked, so the customer never gets it: stranded money. Our ledger stays internally consistent (no balance without a booked payment), which is exactly why internal reconciliation cannot see the problem; only the "stuck in `created`" check below, or reconciling against the provider's own statement, catches it. This gap has been present since the API first published events. It is narrowed, not closed, by re-publishing on duplicates (a provider that gets no response, or a `503`, retries, and the retry re-announces a payment still `created`); it remains for a provider that never retries. It is closed in a later slice with a transactional outbox.
- Approve/reject is random, not a real rules engine.
- No dead-letter handling: when a message exhausts the worker consumer's `max_deliver` (100), JetStream drops it and only emits an advisory (`$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.PAYMENTS.worker`), which nothing consumes yet; messages the worker terminates (invalid events, invariant violations) are likewise only logged and recorded in `event_deliveries`. The payment is left where it stopped, possibly holding funds in `pending_in`. Reconciliation is what surfaces it; the evolution is a dead-letter consumer of those advisories (and of terminated messages) with a replay tool.
- No authentication on the API: anyone who can reach it can read any account's balance and transactions. In particular, `POST /payments` does not verify the sender: real webhooks are authenticated, typically by an HMAC signature header checked against a shared secret.
- No authentication or authorization on NATS: any client that can reach port 4222 can publish to `payment.created` or manage streams. The evolution is a NATS user per service with publish/subscribe allow lists (the API may publish only `payment.created`; the worker may only fetch from and acknowledge its own consumer; only `nats-setup` may use `$JS.API.>`), per-client inbox prefixes so replies aren't readable by other clients, and `deny_delete` / `deny_purge` on the stream.
- One database user for everything (migrations, API, worker, tests), owning all tables. Append-only audit tables are enforced by triggers instead of privileges; the evolution is a role per service with least-privilege grants (the API without write access to balances, the worker unable to `UPDATE` or `DELETE` audit rows) and migrations under a separate owner role.
- No health endpoint. Nothing waits on the API in compose (it is the last service to start), so ordering doesn't need one; a `GET /health` arrives with CI or a load balancer, and goes into the contract then.
- `POST /payments` uses demo status codes rather than a real webhook contract (see *Webhook semantics*), and does not distinguish accounts outside this app from genuinely unknown ones.
- One IBAN and at most one provider account ID per account, single provider, no lifecycle; see `provider_accounts` under *Data model* for the evolution path.
- **Completed credits only:** `POST /payments` assumes the provider notifies only completed (settled and released) credits. Provider-side pending states (funds held for the provider's own screening) and provider rejections before release are out of scope; consuming them would add an upstream state machine before `created`. Recalls, returns and fraud claims (SEPA R-transactions) happen after settlement, are decided by people, and belong to a separate post-settlement return flow, handled with compensating movements rather than by changing the original payment.

## Decisions

1. **Accounts:** demo accounts seeded by a migration (with test IBANs); no `POST /accounts`.
2. **Tagging:** no tag for slice 1; `v0.1-simple-ledger` after withdrawals.
3. **States:** `created` (set by the API: notified, nothing booked) → `received` (funds booked into `pending_in`) → `under_review` (compliance has picked it up) → `approved | rejected` (the decision) → `credited | returned` (the decision posted). Decisions are separate from their effects: compliance transitions never move money, posting transitions always do; each transition has one owner.
4. **Rejections:** modelled as returned to sender in the ledger; the network return itself is out of scope.
5. **IDs:** UUIDv7 stored as `uuid`, enforced by `CHECK (uuid_extract_version(…) = 7)`; app-generated, with `DEFAULT uuidv7()` on entity primary keys as a fallback only; TypeID-prefixed outside the database. Typeless references carry a `reference_type`. Primary keys: UUIDv7 for entities that may be referenced or exposed; `bigserial` for internal append-only logs. `balance_movements` counts as an entity because it becomes the double-entry transactions table.
6. **Incoming idempotency:** `UNIQUE (network, network_transaction_id)`; no client `Idempotency-Key` until withdrawals.
7. **API is ingestion only:** it never changes balances; all balance movements happen in the worker via the ledger module. *(Revised in slice 2, decision 25: withdrawals hold funds synchronously in the API request, still only through the ledger's posting functions.)*
8. **No check-then-act:** duplicate and race protection comes from DB constraints and row counts, never from a prior `SELECT`.
9. **Queries:** raw parameterised SQL via `pg`, confined to the data-access layer, with explicit typed row mapping.
10. **Ledger is self-protecting:** `balance_movements` with `UNIQUE (reference_type, reference_id, reference_step)`; `move()` is a no-op on a duplicate reference. Payment state drives the workflow but is not the only thing preventing double movements, avoiding coupling the ledger to payment state.
11. **Duplicates are counted, not guarded, by a log:** `event_deliveries` (one row per delivery, no unique constraint) replaces `processed_events`; ingestion duplicates counted via `notification_count`.
12. **Money:** `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON; division via one helper with an explicit rounding mode. Rationale in `docs/decisions/0001-money-representation.md`.
13. **`POST /payments` status codes:** conventional API codes for the demo; the real webhook contract (2xx for anything recorded or handled, 5xx only when a retry helps, accounts outside this app acknowledged and ignored) is documented under *Webhook semantics*. *(Revised in slice 2, decisions 37–38 and step 6b: the endpoint is now `POST /provider/incoming-payments` with the webhook contract; unknown-account payments are recorded in suspense, conflicts as records.)*
14. **Provider identifiers mapped at the edge:** notifications identify accounts by `creditor_iban`, mapped to `account_id` during ingestion; the IBAN as received is stored on the payment; nothing downstream sees provider identifiers. The provider account ID is optional, stored when sent, and not used for matching or for duplicate detection. One IBAN and at most one provider account ID per account for now; `provider_accounts` (per provider, with lifecycle status) is the evolution path.
15. **Event naming:** `payment.<new status>`, one event per state transition, published on entering that status; status names are part of the event contract. Only `payment.created` is published in slice 1.
16. **No request hash:** duplicate detection compares the stored `creditor_iban`, `amount_minor` and `currency` directly in the upsert. Every identifying detail is stored on the row, so a hash would add a cross-language format to keep in sync and hide which field differed.
17. **Stream setup:** JetStream streams (`nats/streams/*.json`) and durable consumers (`nats/consumers/<STREAM>/*.json`) are applied by a one-off compose service, like the migrations; apps only check that they exist. Tests use a separate NATS server (`nats-test`) with the same definitions, as they use `ledger_test`. Stream `PAYMENTS`: subjects `payment.>`, file storage, `limits` retention for 7 days, default 2-minute duplicate window (see *Stream `PAYMENTS`*).
18. **Publish failure:** if `payment.created` can't be published after the insert commits, the API answers `503` (with `Retry-After`), and a duplicate notification of a payment still `created` publishes it again, with a new `event_id`. The provider's retry is what completes the publish until the outbox replaces this. *(Superseded in slice 2, decision 22: events go through the transactional outbox; no 503, no re-publish on duplicates.)*
19. **No health endpoint** in slice 1 (see *Known gaps*).
20. **Message IDs:** `Nats-Msg-Id = <payment_id>.<event>` (e.g. `pay_….created`), one per payment and event type, rather than `event_id`, so the stream's duplicate window drops repeated announcements of the same transition. `event_id` still identifies each event; when the stream drops a repeat, the API logs `stream_duplicate: true` instead of the new, unstored `event_id`.
21. **Worker retries:** invariant violations are terminated by the handler (logged `fatal`, `alert: true`); transient failures are `nak`ed with a growing delay (1 s doubling, at most 60 s), up to the consumer's `max_deliver` of 100. No consumer `backoff`, because it would also shorten `ack_wait` for messages in progress.
