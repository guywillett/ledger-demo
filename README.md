# ledger-demo

A small payment event-processing system (incoming payments, pseudo compliance review, ledger balances) built to demonstrate correctness under duplicates, retries, crashes and concurrency.

See [`docs/slices/`](docs/slices/) for the specs.

## Local development

Everything runs under Docker Compose. Requires Docker with Compose v2 (`docker compose`, not `docker-compose`).

| Service | Purpose | Host port |
|---|---|---|
| `postgres` | PostgreSQL 18 | `5432` |
| `nats` | NATS with JetStream | `4222` (clients), `8222` (monitoring) |
| `migrate` | dbmate: applies `db/migrations/` and exits | — |
| `migrate-test` | the same migrations against the test database `ledger_test` | — |
| `nats-setup` | nats-box: applies the stream definitions in `nats/streams/` and exits | — |
| `api` | the Node API (`node/`, image `ledger-demo-node`) | `3000` |

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

`migrate` runs once on `up` after Postgres is healthy, applies any pending migrations, and exits; `nats-setup` does the same for the JetStream streams once NATS is healthy. It's normal for both to show as exited. The `api` starts only after both have succeeded.

There's no health endpoint yet (see *Known gaps* in the slice spec); `docker compose logs api` shows "Server listening" once it's up.

### Streams

JetStream streams are defined in `nats/streams/<NAME>.json` (the server's own JSON format; durations in nanoseconds) and applied by `nats/setup.sh`: created if missing, updated if the file changed. To change a stream, edit its file and run `docker compose up -d` (or `docker compose run --rm nats-setup`).

```sh
NATS="docker run --rm --network ledger-demo_default natsio/nats-box:0.20.0 nats -s nats://nats:4222"
$NATS stream info PAYMENTS                       # configuration and message counts
$NATS stream get PAYMENTS --last-for payment.created   # the latest payment.created event
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

Tests default to the compose services on `localhost`; set `DATABASE_URL` / `NATS_URL` to point elsewhere. Every API test response is also checked against `contracts/openapi.yaml`, and every published event against its schema in `contracts/events/`. Tests need Postgres and NATS (with the stream set up), i.e. `docker compose up -d`.

#### Running the API locally

`docker compose up -d` already runs the API on port 3000. To run it on the host instead (e.g. with `--watch`), stop the container first: both want host port 3000.

```sh
docker compose stop api         # free port 3000
cp .env.example .env            # once; .env is gitignored and loaded by Node itself
npm run dev:api                 # restarts on file changes (node --watch)
npm run start:api               # without watching
docker compose start api        # afterwards, back to the container
```

The same curl works against either. Record a payment notification for alice (repeat it to see the duplicate handling: `200` and a rising `notification_count`; change the amount to see a `409`). Each `201` publishes a `payment.created` event (see *Streams* above to read it back):

```sh
curl -i -X POST localhost:3000/payments -H 'content-type: application/json' -d '{
  "network_transaction_id": "NTX-0001",
  "creditor_iban": "FR76 1234 5000 0100 0000 0000 193",
  "amount_minor": "1250",
  "currency": "EUR",
  "network": "SEPA"
}'
```

#### Lint and format

```sh
npm run check                   # format:check + lint + typecheck (what CI will run)
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
