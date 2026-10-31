# Development

The reference for running the system locally, testing it, and the tooling.

# Local development

Everything runs under Docker Compose. Requires Docker with Compose v2 (`docker compose`, not `docker-compose`).

| Service | Purpose | Host port |
|---|---|---|
| `postgres` | PostgreSQL 18 | `5432` |
| `nats` | NATS with JetStream | `4222` (clients), `8222` (monitoring) |
| `migrate` | dbmate: applies `db/migrations/` and exits | — |
| `migrate-test` | the same migrations against the test database `ledger_test` | — |
| `migrate-go-test` | the same migrations against the Go tests' database `ledger_go_test` | — |
| `migrate-go-relay-test` | the same again against `ledger_go_relay_test`, the Go outbox and relay tests' own database | — |
| `nats-setup` | nats-box: applies the stream and consumer definitions in `nats/` and exits | — |
| `api` | the Node API (`node/`, image `ledger-demo-node`); Postgres only, no NATS | `3000` |
| `relay` | the Node relay (same image): publishes the outbox to JetStream (`DEPOSITS`, `WITHDRAWALS`) | — |
| `deposit-worker` | the Node deposit-worker (same image): books, reviews and posts deposits | — |
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

## Start and stop

```sh
docker compose up -d            # start everything in the background (migrations and streams set up automatically)
docker compose up -d --build    # the same, rebuilding the Node image after code changes
docker compose ps -a            # status; `migrate` and `nats-setup` should show "Exited (0)"
docker compose logs -f          # follow all logs (or: docker compose logs -f nats)
docker compose stop             # stop containers, keep them and their data
docker compose down             # remove containers and network; data volumes are kept
```

`migrate` runs once on `up` after Postgres is healthy, applies any pending migrations, and exits; `nats-setup` does the same for the JetStream streams once NATS is healthy. It's normal for both to show as exited. The `api` starts only after `migrate` has succeeded; the `relay`, `deposit-worker` and `submitter` after both (the `submitter` also after `provider-sim` has started).

There's no health endpoint yet (see *Known gaps* in the slice spec); `docker compose logs api` shows "Server listening" once it's up.

## Streams and consumers

JetStream streams are defined in `nats/streams/<NAME>.json` and durable consumers in `nats/consumers/<STREAM>/<NAME>.json` (the server's own JSON format; durations in nanoseconds). `nats/setup.sh` applies them: created if missing, updated if the file changed; changes the server refuses (e.g. a consumer's `deliver_policy`) fail loudly. To change one, edit its file and run `docker compose up -d` (or `docker compose run --rm nats-setup`).

```sh
NATS="docker run --rm --network ledger-demo_default natsio/nats-box:0.20.0 nats -s nats://nats:4222"
$NATS stream info DEPOSITS                       # configuration and message counts
$NATS stream get DEPOSITS --last-for deposit.created   # the latest deposit.created event
$NATS stream get WITHDRAWALS --last-for withdrawal.held # the latest withdrawal.held event (IDs and amount only)
$NATS consumer info DEPOSITS deposit-worker              # the deposit-worker's consumer: pending, unacked, redelivered
```

## Fake provider

`provider-sim` stands in for the BaaS the withdrawals go out through (test infrastructure; see *Fake provider* in the [slice 2 spec](slices/slice-2-outbox-and-withdrawals.md)). `POST /transfers` is idempotent on `Idempotency-Key`; each transfer's result (`executed` or `failed`) follows after `SIM_EXECUTION_DELAY_MS`. Results go to the API's webhook (`SIM_WEBHOOK_URL`), resent until it answers 2xx. Failure modes are chosen by the start of `remittance_info`: `SIM REFUSE`, `SIM ERROR`, `SIM SLOW`, `SIM FAIL`, `SIM DUPLICATE`, `SIM EARLY`.

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

## Relay

`docker compose up -d` starts one relay. It claims unpublished events from the `outbox` table in batches (`RELAY_BATCH_SIZE`, default 10, leased for `RELAY_LEASE_MS`, default 30 s), publishes each with its `msg_id` as the `Nats-Msg-Id`, and marks it published; a failed publish is retried with backoff. When it has caught up it polls every 200 ms to 1 s.

```sh
docker compose logs -f relay               # one "event published" line per event
docker compose up -d --scale relay=2       # two instances: each claims its own batches
docker compose exec postgres psql -U ledger -d ledger -c \
  'SELECT subject, msg_id, attempts, last_error, published_at FROM outbox ORDER BY id DESC LIMIT 5'
```

## Submitter

`docker compose up -d` starts one submitter. It pulls `withdrawal.held` from the durable `submitter` consumer on `WITHDRAWALS` and sends each withdrawal to the provider (`PROVIDER_URL`, provider-sim in compose) with its TypeID as `Idempotency-Key`, within `PROVIDER_TIMEOUT_MS` (default 5 s). Accepted → `submitted`; refused (`422`) → `failed` with the funds released; an unknown outcome (timeout, `5xx`) → retried later with the same key, the withdrawal still `held`. The provider then reports the result to `POST /provider/transfer-updates`, which settles the withdrawal: `executed` (the held funds leave) or `failed` (they're released).

```sh
docker compose logs -f submitter           # one line per submission and its outcome
docker compose up -d --scale submitter=2   # two instances sharing the consumer
$NATS consumer info WITHDRAWALS submitter  # pending, unacked, redelivered
```

## Deposit-worker

`docker compose up -d` starts one deposit-worker. It pulls `deposit.created` from the durable `deposit-worker` consumer, books the deposit, reviews it (`REVIEW_DELAY_MS`, default 2000), decides (approve with probability `APPROVE_RATE`, default 0.8) and posts the decision, handling up to `DEPOSIT_WORKER_CONCURRENCY` (default 10) messages at once.

```sh
docker compose logs -f deposit-worker              # follow what it does
docker compose up -d --scale deposit-worker=3      # three instances sharing the consumer; each message goes to one
docker compose kill -s SIGKILL deposit-worker      # crash it mid-review: the message is redelivered after ack_wait (10 s)
```

## Migrations

Migrations are plain SQL files in `db/migrations/`, each with a `-- migrate:up` and a `-- migrate:down` section, run in filename order. dbmate records applied versions in the `schema_migrations` table. Each file runs in a single transaction.

```sh
docker compose run --rm migrate status            # list applied / pending migrations
docker compose run --rm migrate up                # apply pending migrations
docker compose run --rm migrate rollback          # undo the most recent migration (runs its down section)
docker compose run --rm migrate new add_something # create db/migrations/<timestamp>_add_something.sql
```

`docker compose run --rm migrate <args>` replaces the service's default command (`up`) with `<args>`; `--rm` removes the one-off container afterwards. Postgres must be running (`docker compose up -d postgres`).

No `schema.sql` dump is written (`DBMATE_NO_DUMP_SCHEMA`): while the schema is still settling we prefer editing migrations and resetting the database (below) to stacking up corrective migrations.

## Reset

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

## Inspecting

```sh
docker compose exec postgres psql -U ledger -d ledger                # psql shell
docker compose exec postgres psql -U ledger -d ledger -c '\dt'       # list tables

curl 'http://localhost:8222/healthz?js-enabled-only=true'           # NATS + JetStream health
curl http://localhost:8222/jsz                                      # JetStream overview (streams, consumers)
```

## Demo accounts

Seeded by `db/migrations/20261018000002_seed_demo_accounts.sql`, with zero balances:

| Name | TypeID (API) | UUID (database) | IBAN (provider notifications) |
|---|---|---|---|
| alice | `acct_01m564xr00fx7rjvqryn5dzwxx` | `01a14c4e-e000-7f4f-896e-f8f54adff3bd` | `FR7612345000010000000000193` |
| bob | `acct_01m564xr01ecrvnckn84evqsax` | `01a14c4e-e001-7331-bab2-75411dbbe55d` | `FR7612345000010000000000290` |
| carol | `acct_01m564xr02e70a25p2hkva3gbn` | `01a14c4e-e002-71c0-a116-c28cf6a1c175` | `FR7612345000010000000000387` |

## Reconciliation

Checks that balances, movements and deposit and withdrawal statuses agree, and reports what a person should look at: stuck deposits and withdrawals, unpublished events, suspense (unmatched deposits, whole-database runs only), notification conflicts and conflicting or unmatched transfer updates (see *Reconciliation* in the slice specs):

```sh
cd node && npm run reconcile                                  # the development database (via .env)
npm run reconcile -- --json --account acct_01m564xr00fx7rjvqryn5dzwxx   # one account, as JSON
npm run reconcile -- --stuck-after 0 --fail-on-stuck          # also fail on anything unfinished
docker compose run --rm reconcile --stuck-after 0                       # from compose, no local Node needed
```

Exit code `0` means the books agree, `1` that they disagree, `2` stuck deposits, withdrawals or events with `--fail-on-stuck`, `3` that it couldn't run. Suspense and conflicts never fail it: they are a person's job, not inconsistent books. Run against `ledger_test` it reports differences by design: some tests leave deliberate inconsistencies behind.

## Node

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

Tests run against their own database, `ledger_test` (migrated by the `migrate-test` service with the same migrations), never the development database `ledger`. Each test creates the accounts and deposits it asserts on, with random IDs and IBANs, so tests don't depend on each other and nothing is deleted afterwards. To start from a clean slate:

```sh
npm run test:db:reset           # drop and re-migrate ledger_test
```

Tests default to the compose services on `localhost`: `ledger_test` and the test NATS (`nats-test`, port 4223). To point them elsewhere, set `TEST_DATABASE_URL` / `TEST_NATS_URL` (and `TEST_LOG_LEVEL` to see logs); the tests deliberately ignore `DATABASE_URL` / `NATS_URL`, so a development value in your shell can't redirect them. Two guards on top: the database name must end in `_test`, and the e2e tests only purge a NATS server named `ledger-demo-test`. Every API test response is also checked against `contracts/openapi.yaml`, and every published event against its schema in `contracts/events/`. Tests need Postgres and the test NATS, i.e. `docker compose up -d`.

One exception to "tests don't depend on each other": the tests that claim from the outbox (`test/relay/`, `test/outbox/`, `test/api/deposit-events.test.ts`). Claiming takes the oldest unpublished events in the whole table, and every test that creates a deposit leaves one behind, so these files form a second Vitest project, `relay`, that runs after all the others and one file at a time; each of their tests first drains the backlog (see `vitest.config.ts`).

```sh
npm run test:e2e                # process-level tests: the real relay, deposit-worker, submitter and provider-sim; kill -9 the deposit-worker and the submitter, restart them (~2 min)
```

The e2e tests drain the test outbox and purge the test stream first; don't run them at the same time as `npm test`.

### Running the API locally

`docker compose up -d` already runs the API on port 3000. To run it on the host instead (e.g. with `--watch`), stop the container first: both want host port 3000.

```sh
docker compose stop api         # free port 3000
cp .env.example .env            # once; .env is gitignored and loaded by Node itself
npm run dev:api                 # restarts on file changes (node --watch)
npm run start:api               # without watching
docker compose start api        # afterwards, back to the container
```

The same curl works against either. Send an incoming payment notification for alice to the provider webhook. It answers `204` with no body whatever the notification did (the webhook contract), so look at the effect in alice's transactions: a new deposit (`created`, then booked and reviewed by the deposit-worker), a rising `notification_count` when you repeat it, an unchanged deposit when you change the amount (recorded as a conflict), and nothing in any account for an IBAN that isn't ours (recorded in suspense; `docker compose run --rm reconcile` lists it). Each new deposit's `deposit.created` event goes to the outbox, and the relay publishes it (see *Streams* above to read it back):

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

### Lint and format

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

## Go

The Go implementation lives in `go/` (slice 3; the conventions are in `CLAUDE.md`). Install any Go 1.21 or later: `go.mod` pins the toolchain (`toolchain go1.27.3`), and with `GOTOOLCHAIN=auto`, the default, `go` downloads and uses that version by itself. Check with `go env GOTOOLCHAIN` (it should print `auto`) and, in `go/`, `go version` (it should print go1.27.3).

golangci-lint is installed as a release binary, at the version pinned in `go/.golangci-lint-version` (the same file CI reads); golangci-lint's docs advise against `go install` and the `tool` directive (see `go/.golangci.yml`):

```sh
cd go
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b "$(go env GOPATH)/bin" "$(cat .golangci-lint-version)"
```

The install script itself is fetched unpinned (golangci-lint's documented method), but the version it installs is pinned; CI uses the SHA-pinned golangci-lint action instead.

```sh
cd go
gofmt -l .                      # list files gofmt would change (gofmt -w . rewrites them)
go vet ./...                    # the compiler's own static checks
golangci-lint run               # the linters in .golangci.yml
go test -race ./...             # every package's tests, with the race detector (needs `docker compose up -d`)
go test -run TestCommits ./internal/db/   # one test
```

govulncheck reports known vulnerabilities that the code can actually reach (in the toolchain or a dependency), and CI fails on any. It has no release binaries; `go install` builds it at the version in `go/.govulncheck-version`, outside this module (it doesn't touch `go.mod`): `go install "golang.org/x/vuln/cmd/govulncheck@$(cat .govulncheck-version)"`, then `govulncheck ./...` in `go/`.

The tests run against their own database, `ledger_go_test` (migrated by `migrate-go-test`), never `ledger` or Node's `ledger_test`. The outbox and relay tests have one of their own, `ledger_go_relay_test` (migrated by `migrate-go-relay-test`): claiming takes the oldest unpublished events in the whole table, so they mustn't see the events other packages' tests leave behind, and the two packages take turns on it (a Postgres advisory lock held for each package's run; see `internal/testdb`). `TEST_DATABASE_URL` and `TEST_RELAY_DATABASE_URL` point them elsewhere, `TEST_NATS_URL` the NATS (default the compose `nats-test`); as in Node, the database names must end in `_test`. The Node and Go suites share the test NATS, so don't run them at the same time.

## Troubleshooting

- **Port already in use** (`5432`, `4222`, `8222`): another Postgres or NATS is running on the host. Stop it, or change the left-hand side of the port mapping in `docker-compose.yml`.
- **`migrate` exited non-zero:** `docker compose logs migrate` shows the failing statement. Fix the migration and run `docker compose up -d` again, or reset the database if a half-edited migration was already applied.
- **Edited a migration that was already applied:** dbmate won't re-run it. Roll it back first (`docker compose run --rm migrate rollback`), or reset the database.
