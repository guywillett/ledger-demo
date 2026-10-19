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

Connection details from the host:

```text
DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable
NATS_URL=nats://localhost:4222
```

Compose prefixes everything with the project name `ledger-demo`, so containers are `ledger-demo-<service>-1` and volumes are `ledger-demo_pgdata` and `ledger-demo_natsdata`.

### Start and stop

```sh
docker compose up -d            # start everything in the background (migrations run automatically)
docker compose ps -a            # status; `migrate` should show "Exited (0)"
docker compose logs -f          # follow all logs (or: docker compose logs -f nats)
docker compose stop             # stop containers, keep them and their data
docker compose down             # remove containers and network; data volumes are kept
```

`migrate` runs once on `up` after Postgres is healthy, applies any pending migrations, and exits. It's normal for it to show as exited.

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

| Name | TypeID (API) | UUID (database) |
|---|---|---|
| alice | `acct_01m564xr00fx7rjvqryn5dzwxx` | `01a14c4e-e000-7f4f-896e-f8f54adff3bd` |
| bob | `acct_01m564xr01ecrvnckn84evqsax` | `01a14c4e-e001-7331-bab2-75411dbbe55d` |
| carol | `acct_01m564xr02e70a25p2hkva3gbn` | `01a14c4e-e002-71c0-a116-c28cf6a1c175` |

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

Tests default to the compose services on `localhost`; set `DATABASE_URL` / `NATS_URL` to point elsewhere.

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
