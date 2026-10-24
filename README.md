# ledger-demo

[![CI](https://github.com/guywillett/ledger-demo/actions/workflows/ci.yml/badge.svg)](https://github.com/guywillett/ledger-demo/actions/workflows/ci.yml)

A small payment system (incoming payments with a pseudo compliance review, client withdrawals sent to a provider, ledger balances) built to demonstrate correctness under duplicates, retries, crashes, concurrency and unknown outcomes: **however often a notification, request, event or provider result is repeated, redelivered, raced or interrupted, and wherever a process crashes, every balance changes exactly once, and no money is sent twice.** Implemented in Node.js/TypeScript; a Go implementation against the same database schema and contracts comes later.

The specs and decisions are in [`docs/`](docs/): [slice 1](docs/slices/slice-1-incoming-payments.md) (incoming payments) and [slice 2](docs/slices/slice-2-outbox-and-withdrawals.md) (transactional outbox, withdrawals, provider webhooks) are built.

```text
Incoming payments
provider ──POST /provider/incoming-payments──► API ──► Postgres, one transaction:
(webhook)                              payment `created` (no balance change)
                                       + payment.created in the outbox
                                       (unknown account: recorded in suspense, no event)
                                               │
                                     relay (any number of instances): claim, publish, mark
                                               │
                                               ▼
                                 JetStream PAYMENTS: payment.created
                                               │
                                               ▼
                                 worker (any number of instances)
                    book ──► start review ──► decide ──► post the decision
                (pending_in)                (approve/reject)   (available, or returned)
                          each step one transaction, through ledger/move()

Withdrawals
client ──POST /accounts/:id/withdrawals (Idempotency-Key)──► API ──► Postgres, one transaction:
                                       withdrawal `held` (available → held)
                                       + withdrawal.held in the outbox
                                               │
                                     relay ──► JetStream WITHDRAWALS: withdrawal.held
                                               │
                                               ▼
                                 submitter ──POST /transfers (same key on every retry)──► provider
                                    held → submitted                                       │
                                                                                           │
provider ──POST /provider/transfer-updates (webhook)──► API ──► Postgres, one transaction:◄─┘
                                       update recorded + withdrawal settled:
                                       executed (held → external) or failed (held → available)

  GET /accounts/:id/balance, /transactions, /withdrawals/:id        reconciliation: do the books agree?
```

Status codes follow the caller: our client's API answers with conventional REST codes (`201`, `200`, `409`, `422`…); provider webhooks get the webhook contract, `204` for anything recorded or deliberately handled and non-2xx only when a retry could help.

## What it demonstrates

| Guarantee | How | Proved by |
|---|---|---|
| A repeated notification changes nothing | `UNIQUE (network, network_transaction_id)` and one upsert statement: same details → a duplicate, counted | `test/api/incoming-payments.test.ts` ("records the same notification sent 5 times once…", "records 10 concurrent identical notifications once"); `scripts/demo.sh` steps 1–2 |
| A notification that can't be booked is recorded, never refused | an unknown account goes into suspense (status `unmatched`: no account, no event, no movement; a retry stays a duplicate of it, resolution is never automatic); the same network ID with other details becomes a conflict record, the payment unchanged; both alerted and reported by reconciliation | `test/api/incoming-payments.test.ts` ("records a valid IBAN that is not ours as unmatched…", "keeps it unmatched after an account with that IBAN is created…", "records a different amount as a conflict…"); `scripts/demo.sh` steps 3–4 |
| An event is never lost between commit and publish | the event is written to `outbox` in the same transaction as the state change; a relay publishes it at least once (claims leased with `FOR UPDATE SKIP LOCKED`, marked after the stream's acknowledgement); the stream drops a repeat by its `msg_id` | `test/api/payment-events.test.ts` and `test/api/withdrawals.test.ts` (one outbox row, same transaction); `test/outbox/outbox.test.ts` (rollback, leases, disjoint concurrent claims); `test/relay/relay.test.ts` ("publishes again after a relay crashed between publishing and marking, and the stream drops the repeat") |
| A repeated or concurrent event moves money once | every step is a conditional transition (`UPDATE … WHERE status = $expected`), never check-then-act | `test/worker/handler.test.ts` (same event twice, two consumers at once) |
| A retried withdrawal request never takes money twice | the client's `Idempotency-Key` is unique per account on the withdrawal itself, so only a request that commits takes it (a `422`, `404` or crash leaves it free); a repeat returns the withdrawal's current state, a reused key with other details is a `409` | `test/api/withdrawals.test.ts` ("answers a repeated request with the same withdrawal…", "creates one withdrawal for 10 concurrent requests with one key", "refuses insufficient funds with 422, storing nothing, and the key stays free"); `scripts/demo.sh` steps 8–9 |
| Insufficient funds is decided atomically with the hold | the hold is a `move(available → held)` in the request's own transaction, and the balance CHECK is the insufficient-funds check (no check-then-act), so concurrent withdrawals can't overspend | `test/ledger/withdrawals.test.ts` and `test/api/withdrawals.test.ts` ("lets exactly one of two concurrent withdrawals that together exceed available…"); `scripts/demo.sh` step 10 |
| An unknown outcome is retried with the same key, never guessed | the submitter sends each withdrawal with its TypeID as `Idempotency-Key`, built only from the stored withdrawal; a timeout or `5xx` leaves it `held` and asks again later; only a definitive refusal releases funds | `test/submitter/provider.test.ts` ("maps a timeout to unknown, and the next call with the same key gets the same, single transfer"); `test/submitter/handler.test.ts`; `test/e2e/submitter.e2e.ts` ("settles once after the submitter is killed mid-call…", `SIM ERROR`); `scripts/demo.sh` step 13 |
| A provider result is applied exactly once, even when it arrives first, twice, or contradicts itself | the update is recorded and the withdrawal settled in one transaction, by a conditional transition from `held` or `submitted`; a resend is counted, a second event for the same result skips, a contradiction (the other final status, another transfer ID) or an unmatched update is recorded and alerted, moving no money | `test/api/transfer-updates.test.ts`; `test/e2e/submitter.e2e.ts` (`SIM EARLY`, `SIM DUPLICATE`, `SIM FAIL`, `SIM REFUSE`); `scripts/demo.sh` steps 11–12 |
| Provider webhooks follow the webhook contract | `204` with no body for everything recorded or deliberately handled (so they are no oracle for what exists), `400` only for an unusable body, `500` when a retry helps | `test/api/incoming-payments.test.ts`, `test/api/transfer-updates.test.ts` (every outcome `204`; "answers 400 (not 415)…") |
| An answer that signals our bug moves no money | a provider `400`/`409`, a transfer-ID mismatch or a ledger invariant violation terminates the message with an alert and leaves the withdrawal `held` for a person | `test/submitter/handler.test.ts` ("treats invalid (400) as our bug…", "terminates a ledger invariant violation with an alert…") |
| Crash safety | each step commits on its own; a redelivery skips what's done, and a recorded decision is never re-made | `test/worker/handler.test.ts` (crash after booking, after deciding); `test/e2e/worker.e2e.ts` and `test/e2e/submitter.e2e.ts` (real `kill -9` mid-review, mid-call); `scripts/demo.sh` steps 5 and 13 |
| Horizontal scaling | instances share one durable JetStream consumer; each fetches only as many messages as it has free slots; relays claim disjoint batches | `test/e2e/worker.e2e.ts` (two workers; long reviews never redelivered); `test/outbox/outbox.test.ts` ("gives two concurrent claims disjoint events (SKIP LOCKED)") |
| The ledger protects itself | `move()` records each movement under `UNIQUE (reference_type, reference_id, reference_step)`; a reused reference with other details is refused; a reference's type and step are checked as a pair, in the database and in the `Movement` type | `test/ledger/move.test.ts`, `test/ledger/withdrawals.test.ts` |
| Audit trail | `balance_movements`, `payment_status_history` and `withdrawal_status_history` are append-only, enforced by triggers | `test/ledger/move.test.ts`, `test/ledger/lifecycle.test.ts` |
| Exact money | integer minor units as `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON ([decision 0001](docs/decisions/0001-money-representation.md)) | amounts above 2^53 in the API, ledger, database and logging tests |
| The books agree | reconciliation checks balances against payment and withdrawal statuses and against the movement journal; reports stuck payments, withdrawals and events, suspense and conflicts | `test/reconciliation/reconcile.test.ts`; the last test of each e2e file; `scripts/demo.sh` |
| One contract for every implementation | hand-written `contracts/openapi.yaml` and `contracts/events/`; every test response and published event is checked against them | `test/contract.ts`, `test/shared/events.test.ts` |

## Design principles

The decisions a reviewer should see first; each is argued in the specs.

- **Status codes follow the caller** ([slice 2 decision 38](docs/slices/slice-2-outbox-and-withdrawals.md)): the client API uses REST codes; provider webhooks use the webhook contract, because a provider retries anything but a 2xx.
- **Events carry IDs, never personal data:** no IBANs, names or remittance text; consumers read details from the database. A stream keeps messages beyond the database's control.
- **Only success consumes an idempotency key** ([slice 2 decision 24](docs/slices/slice-2-outbox-and-withdrawals.md)): failures store nothing, so a transient fault is never replayed as a permanent answer.
- **Unknown outcomes are never guessed** ([slice 2 decision 26](docs/slices/slice-2-outbox-and-withdrawals.md)): a timeout leaves the withdrawal held and asks the provider again with the same key.
- **A provider answer that signals our bug moves no money** ([slice 2 decision 35](docs/slices/slice-2-outbox-and-withdrawals.md)): alert, terminate, leave it for a person.
- **Provider webhooks live under `src/api/webhooks/`** ([slice 2 decision 38](docs/slices/slice-2-outbox-and-withdrawals.md)): the seam where another provider's adapter would plug in, mapping its payloads onto the same provider-neutral ingestion functions.

## Quick start

```sh
docker compose up -d --build     # Postgres, NATS, migrations, stream setup, API, relay, worker, submitter, provider-sim
scripts/demo.sh                  # payments: duplicates, conflicts, suspense, a killed worker, two workers;
                                 # withdrawals: repeats, insufficient funds, provider failures, a killed submitter; then reconciles
curl -s localhost:3000/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/balance    # alice
```

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) does the same from a clean checkout on every push: `npm run check`, the stack built from scratch, the integration and e2e tests, then `scripts/demo.sh`.

`scripts/demo.sh` exits `0` only if every expectation holds and, once everything has finished, the books agree and nothing is stuck (payments, withdrawals held or submitted, unpublished events). That last check covers the whole development database, so something left unfinished by an earlier run fails it too (the report names it); `docker compose down -v` gives a clean slate. The script talks to the system only over HTTP and through compose (reconciliation runs as its own compose service, reading the shared database), so it will work unchanged against the Go implementation. It takes about a minute.

## Known gaps

Deliberate, and described with their evolution path under *Known gaps* in the [slice 1](docs/slices/slice-1-incoming-payments.md) and [slice 2](docs/slices/slice-2-outbox-and-withdrawals.md) specs:

- **Webhook authentication:** no HMAC signature check on the provider webhooks (nor authentication on the client API or NATS; one database user for everything, append-only enforced by triggers rather than per-service roles).
- **No inbox:** webhooks are processed in the request; storing every raw payload first and processing separately would suit heavy processing and audit, but isn't needed for correctness.
- **Webhooks are the fast path, not the guarantee:** a real integration also polls the provider (status checks for withdrawals still `submitted`, discovery of incoming payments) and reconciles against the daily statement.
- **Suspense is a status, not a ledger account:** in production unmatched money is booked into a suspense account so the books explain the bank balance, and customer payments, treasury flows and suspense are kept in separate flows behind an intake table.
- **A single scheme:** field validation (lengths, the SEPA basic character set) is SEPA-specific and partly in database CHECKs; the SEPA country list in `node/src/shared/iban.ts` is not yet verified against the EPC's official list.
- **One provider:** the provider webhooks speak provider-sim's format. With several providers, each gets an adapter, `src/api/webhooks/<provider>/transfer-updates.ts` and so on, that verifies its signature, parses its payload and maps its statuses and error codes onto the same provider-neutral ingestion functions (`recordTransferUpdate`, `recordNotification`); the submitter's provider client would be chosen per withdrawal the same way.
- **No dead-letter handling:** a message that exhausts `max_deliver`, or that a consumer terminates, is only logged; reconciliation shows the payment or withdrawal as stuck.
- **Outbox growth and relay latency:** published outbox rows are kept (pruning or partitioning is the evolution), and the relay polls, so an event can wait up to 1 s (`LISTEN/NOTIFY` is the evolution).
- **Idempotency keys never expire:** real APIs expire them (e.g. after 24 h).
- **Simplifications:** approve/reject is random; one IBAN per account; no health endpoint.

## Local development

Everything runs under Docker Compose. Requires Docker with Compose v2 (`docker compose`, not `docker-compose`).

| Service | Purpose | Host port |
|---|---|---|
| `postgres` | PostgreSQL 18 | `5432` |
| `nats` | NATS with JetStream | `4222` (clients), `8222` (monitoring) |
| `migrate` | dbmate: applies `db/migrations/` and exits | — |
| `migrate-test` | the same migrations against the test database `ledger_test` | — |
| `nats-setup` | nats-box: applies the stream and consumer definitions in `nats/` and exits | — |
| `api` | the Node API (`node/`, image `ledger-demo-node`); Postgres only, no NATS | `3000` |
| `relay` | the Node relay (same image): publishes the outbox to JetStream (`PAYMENTS`, `WITHDRAWALS`) | — |
| `worker` | the Node worker (same image): books, reviews and posts payments | — |
| `submitter` | the Node submitter (same image): sends each held withdrawal to the provider | — |
| `nats-test` | a separate NATS for the tests, so test events never reach the development stream | `4223`, `8223` |
| `nats-setup-test` | the same definitions applied to `nats-test` | — |
| `provider-sim` | a fake BaaS provider (same image): accepts SEPA transfers, reports their results by webhook; failures by trigger (see *Fake provider*) | `4000` |
| `reconcile` | reconciliation on demand (`docker compose run --rm reconcile`); not started by `up` (profile `tools`) | — |

Connection details from the host:

```text
DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable
NATS_URL=nats://localhost:4222
```

Compose prefixes everything with the project name `ledger-demo`, so containers are `ledger-demo-<service>-1` and volumes are `ledger-demo_pgdata` and `ledger-demo_natsdata`.

### Start and stop

```sh
docker compose up -d            # start everything in the background (migrations and streams set up automatically)
docker compose up -d --build    # the same, rebuilding the Node image after code changes
docker compose ps -a            # status; `migrate` and `nats-setup` should show "Exited (0)"
docker compose logs -f          # follow all logs (or: docker compose logs -f nats)
docker compose stop             # stop containers, keep them and their data
docker compose down             # remove containers and network; data volumes are kept
```

`migrate` runs once on `up` after Postgres is healthy, applies any pending migrations, and exits; `nats-setup` does the same for the JetStream streams once NATS is healthy. It's normal for both to show as exited. The `api` starts only after `migrate` has succeeded; the `relay`, `worker` and `submitter` after both (the `submitter` also after `provider-sim` has started).

There's no health endpoint yet (see *Known gaps* in the slice spec); `docker compose logs api` shows "Server listening" once it's up.

### Streams and consumers

JetStream streams are defined in `nats/streams/<NAME>.json` and durable consumers in `nats/consumers/<STREAM>/<NAME>.json` (the server's own JSON format; durations in nanoseconds). `nats/setup.sh` applies them: created if missing, updated if the file changed; changes the server refuses (e.g. a consumer's `deliver_policy`) fail loudly. To change one, edit its file and run `docker compose up -d` (or `docker compose run --rm nats-setup`).

```sh
NATS="docker run --rm --network ledger-demo_default natsio/nats-box:0.20.0 nats -s nats://nats:4222"
$NATS stream info PAYMENTS                       # configuration and message counts
$NATS stream get PAYMENTS --last-for payment.created   # the latest payment.created event
$NATS stream get WITHDRAWALS --last-for withdrawal.held # the latest withdrawal.held event (IDs and amount only)
$NATS consumer info PAYMENTS worker              # the worker's consumer: pending, unacked, redelivered
```

### Fake provider

`provider-sim` stands in for the BaaS the withdrawals go out through (test infrastructure; see *Fake provider* in the [slice 2 spec](docs/slices/slice-2-outbox-and-withdrawals.md)). `POST /transfers` is idempotent on `Idempotency-Key`; each transfer's result (`executed` or `failed`) follows after `SIM_EXECUTION_DELAY_MS`. Results go to the API's webhook (`SIM_WEBHOOK_URL`), resent until it answers 2xx. Failure modes are chosen by the start of `remittance_info`: `SIM REFUSE`, `SIM ERROR`, `SIM SLOW`, `SIM FAIL`, `SIM DUPLICATE`, `SIM EARLY`.

```sh
curl -s -X POST localhost:4000/transfers -H 'content-type: application/json' \
  -H 'Idempotency-Key: wd_01m5jjt69mec4t28vcqmrkmf79' -d '{
  "amount_minor": "2500", "currency": "EUR",
  "creditor_iban": "DE75512108001245126199", "creditor_name": "Jane Doe",
  "remittance_info": "Rent October", "reference": "wd_01m5jjt69mec4t28vcqmrkmf79"
}'                                          # 201 {"transfer_id":"tr_…"}; again: 200, same ID
curl -s -X POST localhost:4000/transfers -H 'content-type: application/json' \
  -H 'Idempotency-Key: refuse-1' -d '{
  "amount_minor": "2500", "currency": "EUR",
  "creditor_iban": "DE75512108001245126199", "creditor_name": "Jane Doe",
  "remittance_info": "SIM REFUSE", "reference": "wd_test"
}'                                          # 422 {"reason":"creditor account closed"}
docker compose logs provider-sim            # the transfers, and their results 2 s later
```

### Relay

`docker compose up -d` starts one relay. It claims unpublished events from the `outbox` table in batches (`RELAY_BATCH_SIZE`, default 10, leased for `RELAY_LEASE_MS`, default 30 s), publishes each with its `msg_id` as the `Nats-Msg-Id`, and marks it published; a failed publish is retried with backoff. When it has caught up it polls every 200 ms to 1 s.

```sh
docker compose logs -f relay               # one "event published" line per event
docker compose up -d --scale relay=2       # two instances: each claims its own batches
docker compose exec postgres psql -U ledger -d ledger -c \
  'SELECT subject, msg_id, attempts, last_error, published_at FROM outbox ORDER BY id DESC LIMIT 5'
```

### Submitter

`docker compose up -d` starts one submitter. It pulls `withdrawal.held` from the durable `submitter` consumer on `WITHDRAWALS` and sends each withdrawal to the provider (`PROVIDER_URL`, provider-sim in compose) with its TypeID as `Idempotency-Key`, within `PROVIDER_TIMEOUT_MS` (default 5 s). Accepted → `submitted`; refused (`422`) → `failed` with the funds released; an unknown outcome (timeout, `5xx`) → retried later with the same key, the withdrawal still `held`. The provider then reports the result to `POST /provider/transfer-updates`, which settles the withdrawal: `executed` (the held funds leave) or `failed` (they're released).

```sh
docker compose logs -f submitter           # one line per submission and its outcome
docker compose up -d --scale submitter=2   # two instances sharing the consumer
$NATS consumer info WITHDRAWALS submitter  # pending, unacked, redelivered
```

### Worker

`docker compose up -d` starts one worker. It pulls `payment.created` from the durable `worker` consumer, books the payment, reviews it (`REVIEW_DELAY_MS`, default 2000), decides (approve with probability `APPROVE_RATE`, default 0.8) and posts the decision, handling up to `WORKER_CONCURRENCY` (default 10) messages at once.

```sh
docker compose logs -f worker              # follow what it does
docker compose up -d --scale worker=3      # three instances sharing the consumer; each message goes to one
docker compose kill -s SIGKILL worker      # crash it mid-review: the message is redelivered after ack_wait (10 s)
```

### Migrations

Migrations are plain SQL files in `db/migrations/`, each with a `-- migrate:up` and a `-- migrate:down` section, run in filename order. dbmate records applied versions in the `schema_migrations` table. Each file runs in a single transaction.

```sh
docker compose run --rm migrate status            # list applied / pending migrations
docker compose run --rm migrate up                # apply pending migrations
docker compose run --rm migrate rollback          # undo the most recent migration (runs its down section)
docker compose run --rm migrate new add_something # create db/migrations/<timestamp>_add_something.sql
```

`docker compose run --rm migrate <args>` replaces the service's default command (`up`) with `<args>`; `--rm` removes the one-off container afterwards. Postgres must be running (`docker compose up -d postgres`).

No `schema.sql` dump is written (`DBMATE_NO_DUMP_SCHEMA`): while the schema is still settling we prefer editing migrations and resetting the database (below) to stacking up corrective migrations.

### Reset

Remove the Postgres data only, keeping NATS, then rebuild the schema from the migrations:

```sh
docker compose rm -sf postgres migrate   # stop and remove the containers using the volume
docker volume rm ledger-demo_pgdata
docker compose up -d
```

Remove everything, including NATS streams and stored messages:

```sh
docker compose down -v                   # -v also deletes the named volumes
docker compose up -d
```

### Inspecting

```sh
docker compose exec postgres psql -U ledger -d ledger                # psql shell
docker compose exec postgres psql -U ledger -d ledger -c '\dt'       # list tables

curl 'http://localhost:8222/healthz?js-enabled-only=true'           # NATS + JetStream health
curl http://localhost:8222/jsz                                      # JetStream overview (streams, consumers)
```

### Demo accounts

Seeded by `db/migrations/20261018000002_seed_demo_accounts.sql`, with zero balances:

| Name | TypeID (API) | UUID (database) | IBAN (provider notifications) |
|---|---|---|---|
| alice | `acct_01m564xr00fx7rjvqryn5dzwxx` | `01a14c4e-e000-7f4f-896e-f8f54adff3bd` | `FR7612345000010000000000193` |
| bob | `acct_01m564xr01ecrvnckn84evqsax` | `01a14c4e-e001-7331-bab2-75411dbbe55d` | `FR7612345000010000000000290` |
| carol | `acct_01m564xr02e70a25p2hkva3gbn` | `01a14c4e-e002-71c0-a116-c28cf6a1c175` | `FR7612345000010000000000387` |

### Reconciliation

Checks that balances, movements and payment and withdrawal statuses agree, and reports what a person should look at: stuck payments and withdrawals, unpublished events, suspense (unmatched payments, whole-database runs only), notification conflicts and conflicting or unmatched transfer updates (see *Reconciliation* in the slice specs):

```sh
cd node && npm run reconcile                                  # the development database (via .env)
npm run reconcile -- --json --account acct_01m564xr00fx7rjvqryn5dzwxx   # one account, as JSON
npm run reconcile -- --stuck-after 0 --fail-on-stuck          # also fail on anything unfinished
docker compose run --rm reconcile --stuck-after 0                       # from compose, no local Node needed
```

Exit code `0` means the books agree, `1` that they disagree, `2` stuck payments, withdrawals or events with `--fail-on-stuck`, `3` that it couldn't run. Suspense and conflicts never fail it: they are a person's job, not inconsistent books. Run against `ledger_test` it reports differences by design: some tests leave deliberate inconsistencies behind.

### Node

The Node.js implementation lives in `node/`. The Node version is in `.nvmrc` (major version only); `package.json` `engines` and `engine-strict` in `node/.npmrc` make npm refuse other majors.

```sh
nvm install                     # once: installs the version in .nvmrc
nvm use                         # in each new shell (or set up nvm's shell hook)
cd node
npm ci                          # install exactly what package-lock.json records
npm run typecheck               # tsc --noEmit
npm test                        # typecheck, then Vitest (needs `docker compose up -d`)
npm run test:watch              # Vitest in watch mode
```

TypeScript is run directly by Node (type stripping); there is no build step and no `dist/`. `tsc` only type-checks, which is why `npm test` runs it first. Consequences: relative imports use `.ts` extensions, and only erasable syntax is allowed (no `enum`, namespaces or constructor parameter properties; `erasableSyntaxOnly` enforces this).

Tests run against their own database, `ledger_test` (migrated by the `migrate-test` service with the same migrations), never the development database `ledger`. Each test creates the accounts and payments it asserts on, with random IDs and IBANs, so tests don't depend on each other and nothing is deleted afterwards. To start from a clean slate:

```sh
npm run test:db:reset           # drop and re-migrate ledger_test
```

Tests default to the compose services on `localhost`: `ledger_test` and the test NATS (`nats-test`, port 4223). To point them elsewhere, set `TEST_DATABASE_URL` / `TEST_NATS_URL` (and `TEST_LOG_LEVEL` to see logs); the tests deliberately ignore `DATABASE_URL` / `NATS_URL`, so a development value in your shell can't redirect them. Two guards on top: the database name must end in `_test`, and the e2e tests only purge a NATS server named `ledger-demo-test`. Every API test response is also checked against `contracts/openapi.yaml`, and every published event against its schema in `contracts/events/`. Tests need Postgres and the test NATS, i.e. `docker compose up -d`.

One exception to "tests don't depend on each other": the tests that claim from the outbox (`test/relay/`, `test/outbox/`, `test/api/payment-events.test.ts`). Claiming takes the oldest unpublished events in the whole table, and every test that creates a payment leaves one behind, so these files form a second Vitest project, `relay`, that runs after all the others and one file at a time; each of their tests first drains the backlog (see `vitest.config.ts`).

```sh
npm run test:e2e                # process-level tests: the real relay, worker, submitter and provider-sim; kill -9 the worker and the submitter, restart them (~2 min)
```

The e2e tests drain the test outbox and purge the test stream first; don't run them at the same time as `npm test`.

#### Running the API locally

`docker compose up -d` already runs the API on port 3000. To run it on the host instead (e.g. with `--watch`), stop the container first: both want host port 3000.

```sh
docker compose stop api         # free port 3000
cp .env.example .env            # once; .env is gitignored and loaded by Node itself
npm run dev:api                 # restarts on file changes (node --watch)
npm run start:api               # without watching
docker compose start api        # afterwards, back to the container
```

The same curl works against either. Send a payment notification for alice to the provider webhook. It answers `204` with no body whatever the notification did (the webhook contract), so look at the effect in alice's transactions: a new payment (`created`, then booked and reviewed by the worker), a rising `notification_count` when you repeat it, an unchanged payment when you change the amount (recorded as a conflict), and nothing in any account for an IBAN that isn't ours (recorded in suspense; `docker compose run --rm reconcile` lists it). Each new payment's `payment.created` event goes to the outbox, and the relay publishes it (see *Streams* above to read it back):

```sh
curl -i -X POST localhost:3000/provider/incoming-payments -H 'content-type: application/json' -d '{
  "network_transaction_id": "NTX-0001",
  "creditor_iban": "FR76 1234 5000 0100 0000 0000 193",
  "amount_minor": "1250",
  "currency": "EUR",
  "network": "SEPA"
}'
curl -s 'localhost:3000/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/transactions?limit=1'   # the effect
```

Read an account (alice here; TypeIDs of the demo accounts are listed under *Demo accounts*):

```sh
curl -s localhost:3000/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/balance
curl -s 'localhost:3000/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/transactions?limit=10&include=history'
# next page: add &before=<next_before from the previous response>
```

Request a withdrawal from alice (repeat it with the same key to get `200` and the same withdrawal; change the amount to see a `409`; ask for more than `available` to see a `422`), then read it back with the `Location` it returns. Within a few seconds the submitter sends it to provider-sim (`submitted`) and the provider's result settles it: `executed`, with its amount gone from `held`; put `SIM FAIL` in `remittance_info` to see it `failed` with the funds back in `available`:

```sh
curl -i -X POST localhost:3000/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/withdrawals \
  -H 'content-type: application/json' -H 'Idempotency-Key: rent-2026-10' -d '{
  "amount_minor": "2500",
  "currency": "EUR",
  "creditor_iban": "DE75 5121 0800 1245 1261 99",
  "creditor_name": "Jane Doe",
  "remittance_info": "Rent October"
}'
curl -s 'localhost:3000/withdrawals/wd_…?include=history'
```

The provider's webhook can be called by hand too. Every update it can parse gets `204` with no body, whatever it did (applied, a duplicate, a contradiction, an unknown reference); `docker compose logs api` and the `transfer_updates` table show what happened:

```sh
curl -i -X POST localhost:3000/provider/transfer-updates -H 'content-type: application/json' -d '{
  "provider_event_id": "pev_manual_1",
  "transfer_id": "tr_manual",
  "reference": "wd_…",
  "status": "executed",
  "occurred_at": "2026-10-24T09:00:00Z"
}'
```

#### Lint and format

```sh
npm run check                   # format:check + lint + typecheck (CI's first step)
npm run format                  # Prettier: rewrite files in place
npm run lint                    # oxlint, including type-aware rules
```

- **Prettier** (`.prettierrc.json`) formats everything under `node/`.
- **oxlint** (`.oxlintrc.json`) lints. Type-aware rules (`no-floating-promises`, `no-misused-promises`, …) run through `oxlint-tsgolint`, which uses the same TypeScript 7 checker as `tsc`. ESLint's typescript-eslint doesn't support TypeScript 7 yet.
- Exported functions must declare parameter and return types (`explicit-module-boundary-types`; not enforced in tests).
- A `switch` over a union type must list every member (`switch-exhaustiveness-check`); a `default:` alone doesn't count, so adding a new status flags every switch that misses it.
- Editor: install the Prettier and oxc extensions; `.editorconfig` at the repo root covers indentation and line endings for all languages.

### Troubleshooting

- **Port already in use** (`5432`, `4222`, `8222`): another Postgres or NATS is running on the host. Stop it, or change the left-hand side of the port mapping in `docker-compose.yml`.
- **`migrate` exited non-zero:** `docker compose logs migrate` shows the failing statement. Fix the migration and run `docker compose up -d` again, or reset the database if a half-edited migration was already applied.
- **Edited a migration that was already applied:** dbmate won't re-run it. Roll it back first (`docker compose run --rm migrate rollback`), or reset the database.
