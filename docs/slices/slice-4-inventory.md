# Slice 4 preparation: inventory of the client API, its workarounds and its data

**A snapshot at `v0.3-go`, not kept up to date.** Made before writing the [slice 4 spec](slice-4-api.md), as the evidence for its scope: which workarounds the client API's gaps cause. The "planned shape" below is the brief's, before the spec settled it (the spec drops the statement and adds the combined `/transactions`); line numbers are as of `v0.3-go`.

Read-only inventory of `main` at `17a37a2` (Merge slice 3). No proposals beyond naming the endpoint a workaround would need. "Planned shape" means the endpoints in the brief: `GET /accounts/{id}`, `…/balance`, `…/deposits` (list, by status), `…/deposits/{id}`, `…/withdrawals` (list, by status), `…/withdrawals/{id}`, `POST …/withdrawals`, `…/statement`.

Line numbers are for the files as they stand on `main`.

---

## 1. Workarounds

### Counts

**Scripts:** 16 workarounds.

| Category | Count | a | b | c |
|---|---|---|---|---|
| Reading the database (directly, or through reconciliation) | 5 | 1 | 0 | 4 (one of them partly a) |
| Reading logs (our services) | 3 | 0 | 0 | 3 |
| Reading provider-sim's logs or state | 3 | 0 | 2 | 1 |
| Listing more than needed, then filtering | 3 | 3 (two only with a filter the planned shape lacks) | 0 | 0 |
| Computing or hard-coding IDs | 2 | 2 (one only in part) | 0 | 0 |

**Tests (patterns, Node and Go together):** 20 patterns.

| Category | Patterns | Call sites in API and e2e tests (Node / Go) |
|---|---|---|
| Database reads the client API **already answers today** | 3 | 34 / 24 |
| Database reads a planned endpoint would answer | 4 | 52 / 37 |
| Database reads that are legitimately internal (c) | 6 | listed per pattern below |
| Log reads (provider-sim's among them, b; the rest c) | 4 | listed per pattern below |
| Listing and filtering | 0 | none found |
| Computed IDs | 2 | a few; mostly c |
| Direct set-up (allowed, c) | 1 | throughout |

A test that reads the database (`balancesOf`, `statusOf`, `getWithdrawal`, `movementSteps`, `outboxRowsFor`, raw `pool.query`) below the API layer is c by the brief's own rule. Those are counted at the end of the tests section, not listed. In Node that means `ledger/`, `deposit-worker/`, `submitter/`, `relay/`, `outbox/`, `reconciliation/` and `shared/`; in Go, every package but `api`.

### 1.1 Scripts

#### Reading the database (5)

| # | Where | What it learns | Why the API can't tell it today | Class |
|---|---|---|---|---|
| S1 | `scripts/differential.sh:199-204` | Every withdrawal's status and ID: `psql … SELECT status, id FROM withdrawals ORDER BY id` | Withdrawals can't be listed (only `GET /withdrawals/{id}` with a known ID). The script's own comment says so, at 199. | **a**: `GET /accounts/{id}/withdrawals` (by status), once per demo account |
| S2 | `scripts/demo.sh:228-229` | That the unmatched notification is in suspense: the reconciliation report (which reads the database) contains the network ID | Suspense deposits have no account, so no account-scoped endpoint shows them | **c**: suspense is the operator's business, not the client's |
| S3 | `scripts/demo.sh:255-266` | That every deposit has finished: `reconcile --stuck-after 0 --fail-on-stuck` until exit ≠ 2 | Also covers unpublished outbox events and the whole database, earlier runs included (header, lines 27-29) | **c**, and **a in part**: `GET /accounts/{id}/deposits?status=<a non-final status>` per demo account answers the deposit half, not the outbox half |
| S4 | `scripts/demo.sh:352-357` | That the books agree and nothing is stuck | Reconciliation's job | **c** |
| S5 | `scripts/shared-consumer.sh:135-139` | The same | The same | **c** |

#### Reading logs (3)

| # | Where | What it learns | Why the API can't tell it | Class |
|---|---|---|---|---|
| S6 | `scripts/demo.sh:119-134` (used at 346-347) | The submitter's delivery attempt when it found the withdrawal executed (it expects 2) | Delivery attempts are internal | **c** |
| S7 | `scripts/shared-consumer.sh:72-76` | That `deposit-worker-other` has started (its "deposit-worker started" line) | Workers have no HTTP surface and no health endpoint | **c** |
| S8 | `scripts/shared-consumer.sh:102-133` | Which implementation processed each deposit, and that none was processed twice ("deposit processed" lines, by `implementation`) | Which process handled an event is internal | **c** |

#### Reading provider-sim (3)

| # | Where | What it learns | Why | Class |
|---|---|---|---|---|
| S9 | `scripts/demo.sh:106-117` (used at 334) | That provider-sim has created the transfer for a withdrawal (its "transfer created" line with `reference`), so the submitter can be killed mid-call | provider-sim has only `POST /transfers` | **b**: whether a transfer exists for a reference (and its `transfer_id`). Its state already holds this (§4) |
| S10 | `scripts/demo.sh:136-142` (used at 350) | How many transfer requests provider-sim received for a reference ("transfer created" plus "repeated transfer request" lines), expecting 1 | The same | **b**: a request count per reference. **Not in provider-sim's state today**: a repeat is only logged (§4) |
| S11 | `scripts/demo.sh:330, 348-349` | That provider-sim is the same container throughout (`docker compose ps -q provider-sim`), so its log holds its whole history | provider-sim keeps state in memory only | **c**: a precondition of S10. An endpoint answering from memory would need the same guard |

#### Listing more than needed, then filtering (3)

| # | Where | What it learns | Why | Class |
|---|---|---|---|---|
| S12 | `scripts/demo.sh:65-71` (`deposit()`: `GET /accounts/{id}/transactions?limit=200`, then a grep for the network ID). Used at 156 (`fund`, polled every second), 200-201, 211-213, 218-220 and 224-226 (absence in each account's list) | A deposit by its network transaction ID: status, `notification_count`, `amount_minor`, `id` | The webhook answers 204 with no body (by contract, so it isn't an oracle), so the caller never gets a deposit ID. No endpoint looks a deposit up by network ID. Assumes the deposit is among the newest 200 (comment at 66). | **a only with a filter the planned shape doesn't have**: `GET /accounts/{id}/deposits?network_transaction_id=…`. The planned list alone keeps the list-and-filter, and `…/deposits/{id}` needs the ID the script doesn't have |
| S13 | `scripts/shared-consumer.sh:45-50` (used at 93-94) | The same, for 30 deposits polled every 2 s; also collects each `id` | The same | The same as S12 |
| S14 | `scripts/differential.sh:171-197` | Every deposit's status across all pages (limit 200), to refuse to run while any is non-final; the first deposit ID; whether some account needs several pages of 7 | Listing every deposit is legitimate here for the IDs and page count. The "is any non-final" check is a list-and-scan | **a** for the status check: `GET /accounts/{id}/deposits?status=…` |

#### Computing or hard-coding IDs (2)

| # | Where | What | Why | Class |
|---|---|---|---|---|
| S15 | `scripts/differential.sh:137-160`, used at 209 | Converts a UUID read with psql into a TypeID suffix (awk: Crockford base32) | S1 reads UUIDs from the database | **a**: goes with S1 (`GET /accounts/{id}/withdrawals` returns TypeIDs) |
| S16 | `scripts/demo.sh:42-47`, `scripts/shared-consumer.sh:20-25`, `scripts/differential.sh:30-32`; Node `test/helpers.ts:29-41` | The seed accounts' TypeIDs and IBANs, copied from the seed migration's comment (`db/migrations/20261018000002_seed_demo_accounts.sql:4-7`) | No endpoint returns an account's IBAN | **a for the IBANs**: `GET /accounts/{id}` (the `iban` column). The account IDs stay constants: an account list isn't planned |

Not workarounds by the brief's definition, listed for completeness:
- `demo.sh:99` and `differential.sh:281-293, 319` read `GET /withdrawals/{id}`, which changes in the clean break (§5).
- `wait-for-stack.sh:17` waits on compose's one-shot setup containers with `docker wait`. That's orchestration.

### 1.2 Tests

#### Database reads the client API already answers today (3 patterns)

| Pattern | Node call sites | Go call sites | What it learns | Class |
|---|---|---|---|---|
| Balance from `account_balances` (`balancesOf`) in API and e2e tests | `api/withdrawals.test.ts` 6, `api/transfer-updates.test.ts` 4, `e2e/deposit-worker.e2e.ts` 3, `e2e/submitter.e2e.ts` 3 (16) | `api/withdrawals_test.go` 6, `api/webhook_transfer_updates_test.go` 4 (10) | The three buckets | Exists: `GET /accounts/{id}/balance`. `api/withdrawals.test.ts:92` and `api/withdrawals_test.go:202` already use it beside `balancesOf` |
| Withdrawal status and failure reason through the data-access function (`getWithdrawal` / `withdrawals.Get`) | `api/transfer-updates.test.ts` 7, `e2e/submitter.e2e.ts:67-69` (its `statusOf`, 7 calls) and `:131, :146` (14) | `api/webhook_transfer_updates_test.go` `storedWithdrawal` 7 | `status`, `failure_reason` | Exists: `GET /withdrawals/{id}`. **But** `providerTransferId` is also read (`e2e/submitter.e2e.ts:117`; Node `api/transfer-updates.test.ts`, 3; Go `.ProviderTransferID`, 4), which the response deliberately leaves out (`node/src/api/schemas.ts:153`): that part is **c** |
| Number of history rows (`historyCount`) | `api/incoming-payments.test.ts` 4 | `api/webhook_test.go` 7 (3 of them in an error message) | One history row after repeats | For matched deposits, exists: `include=history` on the account's list. To find the deposit in the list needs the S12 filter. For unmatched deposits, **c** |

#### Database reads a planned endpoint would answer (4 patterns)

| Pattern | Node | Go | What it learns, and why the API can't tell it | Class |
|---|---|---|---|---|
| Stored deposit by network ID (`depositByNetworkId` / `depositByNetworkID`, `mustDeposit`) | `api/incoming-payments.test.ts` 18, `api/deposit-events.test.ts` 1, `e2e/deposit-worker.e2e.ts:50` 1 (20) | `api/webhook_test.go` 21 | Status, `account_id`, amount, `notification_count`, `id`, after a 204 that returns nothing (as S12) | Matched: **a only with** `GET /accounts/{id}/deposits?network_transaction_id=…` (not planned). Unmatched (`account_id` null): **c** |
| Deposit status by ID (`statusOf`) in e2e | `e2e/deposit-worker.e2e.ts` 6 | none (no Go e2e) | Status while waiting (`credited`, `under_review`) | **a**: `GET /accounts/{id}/deposits/{id}` |
| Number of withdrawals per account (`withdrawalsOf`) | `api/withdrawals.test.ts` 6 | `api/withdrawals_test.go` 7 | "One withdrawal stored", "nothing stored" | **a**: `GET /accounts/{id}/withdrawals` |
| Movements by reference: `holdsOf`, `stepsOf`, `movementSteps`, and `api/incoming-payments.test.ts:273-277` | `api/withdrawals.test.ts` 3, `api/transfer-updates.test.ts` 6, `e2e/submitter.e2e.ts` 7, `e2e/deposit-worker.e2e.ts` 3, `api/incoming-payments.test.ts` 1 (20) | `api/withdrawals_test.go` holdsOf 3, `api/webhook_transfer_updates_test.go` stepsOf 6 (9) | The steps applied (`hold`, `execute`, `book`, `credit`), in order, none twice | **a only if** `GET /accounts/{id}/statement` rows carry reference type, ID and step. Otherwise **c** (the ledger's journal) |

#### Database reads that are legitimately internal (c) (6 patterns)

| Pattern | Node | Go | Why it's c |
|---|---|---|---|
| Outbox rows and counts: `outboxRows`, `outboxRowsFor`, `heldEventsOf`, `deposit-events.test.ts:50`, the `xmin` same-transaction checks (`deposit-events.test.ts:92`, `withdrawals.test.ts:341`) | 3 + 6 + 4 + 1 + 2 | `outboxCount` 7, `outboxRowsFor` 3, `heldEventsOf` 4, xmin `withdrawals_test.go:559` | The outbox is internal; "same transaction" is internal behaviour |
| Conflict records (`conflictsOf`) | 2 | 2 | Recorded and alerted for a person; the webhook deliberately says nothing |
| Transfer-update records (`rowOf`; `e2e/submitter.e2e.ts:197-203` counts a resend) | 11 + 1 | `updateRowOf` 11 | Provider webhook bookkeeping |
| Delivery log (`deliveriesFor`; `e2e/submitter.e2e.ts:182-189, 222-233`) and JetStream consumer info (`e2e/deposit-worker.e2e.ts:79-83`) | 5 + 2 + 1 | none | Delivery attempts and redeliveries are internal |
| Database CHECKs exercised directly (`api/incoming-payments.test.ts:303-313`) | 1 | `webhook_test.go` has a comment saying the CHECK isn't ported | Testing the schema itself |
| Consistency audit: reconciliation (`reconcile()` at the end of each e2e file and in `api/withdrawals.test.ts:433`) and Go's `expectConsistent` (`api/withdrawals_test.go:683`, 4 + 2 uses) | 3 | 6 | Reconciliation's job (slice 3 decision 50) |

#### Log reads (4 patterns)

| Pattern | Where | Class |
|---|---|---|
| The API's own log lines (`capturingLogger`, `logOf`, `logLine` / `logLines`): outcome messages, `alert`, the access line, no PII | Node `api/incoming-payments.test.ts` (`logOf` 4, plus 419-494), `api/transfer-updates.test.ts` (`logOf` 4, 167, 198, 322), `api/withdrawals.test.ts:355+`, `api/accounts.test.ts`. Go `webhook_test.go` (logLine 9, logLines 4), `withdrawals_test.go` (2 + 1), `webhook_transfer_updates_test.go` (1 + 1), `http_test.go` (2 + 2) | **c**: the log is the behaviour under test |
| Child processes' logs in e2e: "deposit processed" per worker (`e2e/deposit-worker.e2e.ts:171-175`), "event published" and `stream_duplicate` (207-214), "shutting down" (117, 214) | Node only | **c** |
| The submitter's "provider outcome unknown; will retry" (`e2e/submitter.e2e.ts:165-173`) | Node only | **c** as written (the submitter's behaviour). provider-sim's `errored` set holds "this key was answered 503", so **b** could show the same from the provider's side |
| provider-sim's "transfer created" lines (`transfersCreated`: `e2e/submitter.e2e.ts:81-87`, 5 uses; `submitter/provider.test.ts:66-68`, 2 uses, against an in-process sim) | Node only | **b**: transfers by reference (§4). Go's provider tests use an `httptest` fake with its own counter (`go/internal/provider/provider_test.go:22-27, 78`), so they have no such read |

#### Computed IDs (2 patterns)

- **UUID to TypeID for deposits found in the database:** Go `idFromUUID` (`api/webhook_helpers_test.go:259`), used twice in `webhook_test.go` for deposit IDs. Node does the same with `toTypeId` on `depositByNetworkId(...).id`. This goes with the network-ID pattern (**a with the filter**, otherwise it stays).
- **Converting the test's own IDs:** for example `e2e/submitter.e2e.ts:64` takes the response's TypeID to a UUID, then `:82` back to a TypeID. Go's `idFromUUID` for an outbox event ID is at `api/withdrawals_test.go:541`. **c**: trivial conversions, nothing learned.

#### Direct set-up (1 pattern, c)

- **Accounts:** `createTestAccount` inserts them with SQL (`helpers.ts:391-401`; Go `testdb.InsertAccount`, `testdb.go:113-122`). There is no account-creation API, and none is planned.
- **Deposits:** `createTestDeposit` goes through ingestion; Go's `testdb.NewDeposit` uses plain SQL (decision 57).
- **Funding:** `fundTestAccount` calls the ledger's postings. The e2e submitter file funds accounts this way too (`e2e/submitter.e2e.ts:56`), not through the webhook and a worker.
- **Withdrawals:** `createTestWithdrawal` uses `requestWithdrawal`.
- **Others:** a late account insert at `incoming-payments.test.ts:319`.

All of this is allowed set-up for integration tests.

#### Below the API (c, counted only)

Call sites outside `api/` and `e2e/` (definitions excluded):
- **Node:**
  - `balancesOf` 42: ledger/lifecycle 8, ledger/move 13, ledger/withdrawals 10, deposit-worker 5, submitter 5, shared/db 1;
  - `statusOf` 16;
  - `getWithdrawal` 21: ledger/withdrawals 9, submitter 12;
  - `movementSteps` 16;
  - `outboxRowsFor` 14;
  - about 20 raw `pool.query` calls.
- **Go:** database access in 11 non-`api` test files: consumer, db, deposits, depositworker, four in ledger, outbox, relay, submitter.

---

## 2. The API as it is

All six operations, from `contracts/openapi.yaml` (version `0.1.0`), `node/src/api/` and `go/internal/api/`. Both implementations register the same routes (`node/src/api/app.ts:89-91`; `go/internal/api/handler.go:30-35`).

### Client operations

| Operation | Path parameters | Query or headers | Success | Errors |
|---|---|---|---|---|
| `GET /accounts/{account_id}/balance` | `account_id`: `acct_` TypeID | none | 200 `{pending_in, available, held, currency}`: digit strings and `"EUR"`. No `account_id`, no timestamp | 400 `invalid-request` (`errors[].path = "account_id"`); 404 `account-not-found`; 500 |
| `GET /accounts/{account_id}/transactions` | `account_id` | `limit` (1-200, default 50; parsed with JavaScript's `Number()`: whitespace, `0x10`, `1e2` and `01` are accepted, and Go reproduces this in `jsNumber`, `query.go:45-95`, decision 54); `before` (`dep_` TypeID); `include=history`. Unknown parameters are ignored; a repeated parameter is a 400 | 200 `{deposits: [...], next_before: dep_… or null}`. Each deposit has `id, account_id, creditor_iban, provider_account_id, amount_minor, currency, network, network_transaction_id, status, notification_count, created_at, updated_at`, plus `history: [{from_status, to_status, reason, at}]` with `include=history` | 400 `invalid-request` (path errors answered alone first, then query errors); 404 `account-not-found`; 500 |
| `POST /accounts/{account_id}/withdrawals` | `account_id` | `Idempotency-Key` (required, `^[A-Za-z0-9._:-]{1,255}$`; sent twice it is joined with ", " and fails). Strict JSON body: `amount_minor, currency, creditor_iban, creditor_name, remittance_info?` | 201 with `location: /withdrawals/{id}` and a Withdrawal; 200 for a repeat (no `location`) | 400 `invalid-request` (path, header and body errors all at once); 400, 413 or 415 `about:blank` for framing, with the HTTP library's message as `detail`; 404 `account-not-found`; 409 `idempotency-key-conflict` with `differing_fields`; 422 `insufficient-funds` (with `detail`); 500 |
| `GET /withdrawals/{withdrawal_id}` | `withdrawal_id`: `wd_` TypeID | `include=history`; unknown parameters ignored | 200 Withdrawal: `id, account_id, idempotency_key, amount_minor, currency, creditor_iban, creditor_name, remittance_info, status, failure_reason, created_at, updated_at`, plus `history` with `include=history` | 400 `invalid-request` (`withdrawal_id`, then `include`, each alone); 404 `withdrawal-not-found`; 500 |

### Provider webhooks

These are not part of the client API. Both answer 204 with no body for everything handled, 400 for an unusable body, and 500 when a retry can help:
- `POST /provider/incoming-payments`: a tolerant body, 400 type `invalid-notification`;
- `POST /provider/transfer-updates`: 400 type `invalid-transfer-update`.

### Everything else

- **Unknown routes:** a 404 `about:blank`, "No such route.", answered for:
  - unknown paths;
  - other methods on a known path;
  - unclean paths: `//`, a trailing `/`, dot segments, and an empty path parameter (decision 62).
- **HEAD:** answered on every GET route (headers only).
- **Request IDs:** `x-request-id` is echoed or generated on **every** response.
- **Problem shape:** RFC 9457 `{type, title, status, detail?, errors?, differing_fields?}`. Our own types are URNs `urn:ledger-demo:problem:*`; plain HTTP errors are `about:blank` with Node's status phrase as the title (Go maps 413 to "Payload Too Large", `problem.go:107-115`).
- **Formats:** timestamps are ISO 8601 with milliseconds in UTC (`toISOString()`; Go's `timestamp.Time`). IDs are TypeIDs everywhere in paths and bodies. Amounts are digit strings.

### Inconsistencies between operations

1. **Naming.** The path says `transactions`, but the envelope key is `deposits`, and withdrawals aren't in it. The contract schemas are `Transaction`, `TransactionList` and `StatusChange` for deposits, but `Withdrawal` and `WithdrawalStatusChange` for withdrawals.
2. **Scope.** Withdrawals are created under the account (`/accounts/{id}/withdrawals`) but read at the top level (`/withdrawals/{id}`). `location` points to the top level. `GET /withdrawals/{id}` returns any account's withdrawal.
3. **Addressability.** Deposits can be listed but not fetched one at a time (an `id` with no URL). Withdrawals can be fetched but not listed. The account has sub-resources, but `GET /accounts/{id}` is the generic route 404 ("No such route.", `about:blank`), not `account-not-found` (`http_test.go:95`; `differential.sh:296`).
4. **Pagination.** Only the deposit list has it: `limit`, `before` and `next_before`. The cursor is the last item's TypeID, not an opaque token, and there is no `has_more` field: `next_before: null` means the last page, and the implementations fetch `limit + 1` to know that. `before` accepts any well-formed `dep_` ID, another account's or one that doesn't exist, because the query is only `id < $before` (`deposits.ts:123-128`; `read.go:94-97`).
5. **Validation reporting.** The POST reports every problem at once (path, header and body: `withdrawals.ts:30-42`). The GETs answer an invalid path parameter alone, and only then look at the query (`accounts.ts:35-42`, `withdrawals.ts:121-128`; Go the same).
6. **Error types for bad bodies.** A body that fails validation is `urn:…:invalid-request` with `errors`. A body that doesn't parse (malformed JSON, too large, wrong content type) is `about:blank` with the framework's message as `detail` (`errors.ts:17-30`).
7. **Provider identifiers.** A deposit exposes `provider_account_id` and `network_transaction_id`. A withdrawal hides `provider_transfer_id` "so provider identifiers stay at the edge" (`schemas.ts:153`).
8. **Reasons.** A withdrawal has `failure_reason` at the top level. A deposit's rejection reason appears only in its history's `reason`.
9. **The idempotency key** is returned in the Withdrawal body (`idempotency_key`). Deposits expose their provider-side key (`network_transaction_id`).
10. **History.** `include=history` applies to the list (deposits) and to the single withdrawal. The POST never includes it. The contract closes the two shapes differently: `Transaction` uses `allOf` plus `unevaluatedProperties: false`, while `Withdrawal` uses `additionalProperties: false` with `history` among its properties. Observably the same.
11. **The balance** has no `account_id` and no as-of time. The other bodies carry `account_id` and timestamps.
12. **404 mechanisms.** Not observable; they affect queries, not responses:
    - balance: a missing `account_balances` row (`balances.ts:21-23`);
    - transactions: a separate `SELECT 1 FROM accounts` (`deposits.ts:112, 137`);
    - POST: the withdrawals table's foreign-key violation (`withdrawals.ts:77-84`).

### Contract against implementations

- **`x-request-id`:**
  - As a request header, it is documented only on `POST /provider/incoming-payments` and `POST …/withdrawals`.
  - As a response header, it is documented only on incoming-payments' 204 and the withdrawal POST's 201 and 200.
  - Both implementations send it on every response: the GETs, transfer-updates' 204, every error and the 404s.
- **HEAD** on the GET routes is answered by both and not documented.
- **Unknown query parameters** are ignored by both. The contract says this for bodies ("tolerant reader", "Unknown fields are rejected") but not for query parameters. **Repeated query parameters** (a 400) aren't documented either.
- **The `limit` grammar** (JavaScript `Number()`: hex, exponents, whitespace, the empty string is 0, so a 400 "too small") goes beyond the contract's `type: integer`. The contract doesn't say how a non-canonical integer is read.
- **The 413 title** "Payload Too Large" is in neither the contract nor the problem list. It is Node's phrase, which Go copies.

### Node against Go

The only observable differences are the documented ones:
- header-name case (`location` against `Location`), which HTTP treats as equal and the scripts handle (`demo.sh:277-279`, `differential.sh:77-88`);
- Go's 30 s request deadline (decision 58). When it fires, the handler's database call fails and the error path answers 500 (`handler.go:50-56`, `middleware.go:166-172`); Node has no deadline;
- cancelling at shutdown (decision 59), which is not an API matter.

`scripts/differential.sh` found no byte differences in the slice 3 runs (both stacks).

---

## 3. What the database can answer

The schema is in:
- `db/migrations/20261018000001_create_schema.sql` (abbreviated **M1**);
- `20261022000002_create_withdrawals.sql` (**M4**);
- `20261024000002_deposit_suspense_and_conflicts.sql` (**M7**).

### An account row (M1:15-27)

`id` (UUIDv7), `name` (text, `UNIQUE`, not null), `iban` (normalised, `UNIQUE`, format CHECK), `provider_account_id` (nullable, `UNIQUE`), `created_at`.

The table has **no** status, no holder or legal name, no currency and no `updated_at`.
- `name` is a unique label: `alice`, `bob`, `carol` in the seed; `test-<uuid>` in Node tests (`helpers.ts:394-398`); `go-test-<suffix>` in Go tests (`testdb.go:119`).
- Go's test accounts have IBANs `XX00<suffix>`, which pass the format CHECK but not mod-97 (`testdb.go:115-118`). Node's test IBANs are valid (`helpers.ts:369-379`).
- The balance is a separate row, `account_balances` (`pending_in`, `available`, `held` from M4:9-10, `updated_at`), created by a trigger (M1:42-52).

### The planned reads

| Read | Query it needs | Index now |
|---|---|---|
| Account by ID | `SELECT … FROM accounts WHERE id = $1` (with `account_balances` joined on its primary key, if the balance is included) | Yes: the primary keys of `accounts` and `account_balances` (M1:16, M1:30) |
| Deposits per account, newest first, paginated | `WHERE account_id = $1 [AND id < $before] ORDER BY id DESC LIMIT $n` (what `/transactions` runs today) | Yes: `deposits_account_id_idx (account_id, id DESC)` (M1:83-84) |
| … by status | `… AND status = $s …` | Partly: the same index gives the account and the order, and `status` is filtered row by row. No index contains `status`. Suspense rows (`account_id NULL`, M7:15-22) never match an account |
| One deposit by ID and account | `WHERE id = $1 AND account_id = $2` | Yes: the primary key |
| Withdrawals per account, newest first, paginated | `WHERE account_id = $1 [AND id < $before] ORDER BY id DESC LIMIT $n` | Yes: `withdrawals_account_id_idx (account_id, id DESC)`, created in slice 2 "for listing an account's withdrawals" (M4:55-57). Nothing queries it yet |
| … by status | `… AND status = $s …` | Partly, as for deposits |
| One withdrawal by ID and account | `WHERE id = $1 AND account_id = $2` | Yes: the primary key |
| Balance movements per account, paginated | `WHERE account_id = $1 [AND id < $before] ORDER BY id DESC LIMIT $n` | **Only partly:** `balance_movements_account_idx (account_id)` (M1:126-127) finds the account's rows but doesn't give the order, so Postgres would sort the account's movements, or walk the primary key and filter by account. Its other index is the `UNIQUE (reference_type, reference_id, reference_step)` (M1:123) |
| Histories, with `include=history` | `WHERE deposit_id = ANY($1)` and `WHERE withdrawal_id = $1` | Yes: `deposit_status_history_deposit_idx` (M1:97-98) and `withdrawal_status_history_withdrawal_idx` (M4:70-71) |

### A `balance_movements` row (M1:105-124, as changed at M4:91-102)

- **Columns:**
  - `id`: UUIDv7, generated by the application (`newUuid('mov')` in `move.ts:164`; Go `ids.New`);
  - `account_id`;
  - `from_bucket`, `to_bucket`: each one of `external`, `pending_in`, `available`, `held`, and never equal;
  - `amount_minor > 0`;
  - `reference_type`: `deposit` or `withdrawal`;
  - `reference_id`: a UUID with no foreign key;
  - `reference_step`;
  - `created_at`: `DEFAULT now()`, which is the transaction's start time.
- **Reference and step pairs:**
  - deposit: `book` (external → pending_in), `credit` (pending_in → available), `return` (pending_in → external);
  - withdrawal: `hold` (available → held), `execute` (held → external), `release` (held → available).

  These are in `posting.ts:24-33` and `ledger/withdrawals.ts:73-78, 122, 143`. The database checks the pairs (M4:100-102).
- **Running balance:**
  - **Nothing stored.** No column holds the balance after a movement.
  - **Computing it per bucket** means signed sums over the account's movements: a window `SUM` ordered by the movement order, or the current `account_balances` row minus the effects of every newer movement.
  - **Cost:** either way, a page needs every movement on one side of it, older or newer. No index gives `(account_id, id)` order (above).
  - **The order is uncertain:**
    - Commit order isn't recorded.
    - `id` order is when the application generated the ID, before the statement ran.
    - `created_at` order is when the transaction started.
    - For two concurrent transactions on one account, both can differ from the order in which the balance row was changed. A hold whose transaction started before the credit that funded it committed is one such case: the hold's `UPDATE` of `account_balances` waits for the row lock (`move.ts`: insert the movement, then update the balance).
    - Not checked: whether a running sum in `id` order can show a negative bucket for the demo's data.
  - **Movements within one millisecond:** they order by the UUIDv7's random bits unless the generator is monotonic. Neither library's monotonicity was checked here.

---

## 4. provider-sim (`node/src/provider-sim/`, Node only)

- **Endpoints:** only `POST /transfers` (`app.ts:117-204`). Any other path is Fastify's default 404. It has no per-request logging (`app.ts:111-115`). The port is 4000, published on the host (`docker-compose.yml`, "Host port 4000, for curl").
- **Request:**
  - The `Idempotency-Key` header is required.
  - The body is tolerant (`z.object`): `amount_minor, currency, creditor_iban, creditor_name, remittance_info?, reference` (1-35 characters).
  - Both submitters send the withdrawal's TypeID as the key **and** as `reference`: Node `submitter/provider.ts:57, 65, 75`; Go `provider.go:115, 143`. So in practice key = reference.
- **Responses:**

  | Case | Status | Body |
  |---|---|---|
  | New transfer | 201 | `{transfer_id, status: "accepted"}` |
  | Same key and body again | 200 | `{transfer_id, status: "accepted"}` |
  | Same key with another body | 409 | `{error}` |
  | Refused | 422 | `{reason}` |
  | Simulated error (SIM ERROR, first try) | 503 | `{error}` |
  | Invalid request | 400 | `{error, errors}` |

  These are not RFC 9457 problems.

### In memory (`app.ts:101-109`)

- **`transfers: Map<Idempotency-Key, Transfer>`**, each holding:
  - `transferId` (`tr_<uuid>`);
  - `request`: the parsed body, amount as a BigInt;
  - `outcome`: `{status: 'executed'}` or `{status: 'failed', reason}`;
  - `duplicate` (boolean).
- **`errored: Set<key>`**: keys whose first request was answered 503.
- **`refused: Map<key, request>`**: refused requests.
- **`pending: Set<Promise>`**: scheduled results and deliveries; only its size is used.

### Not kept

Each of these is only logged, or not recorded at all:
- the behaviour chosen: `slow`, `early` and "none" leave no trace in state; `fail` shows in `outcome`, `duplicate` in the flag;
- when the transfer was created;
- how many requests arrived for a key (a repeat is logged as "repeated transfer request");
- the `provider_event_id` sent: made per `resultUpdate()` call (`app.ts:222-230`), and reused for the duplicate send (`app.ts:233-237`);
- `occurred_at`;
- whether and when the webhook delivery succeeded, and after how many attempts.

### What a test-only read could return from the current state, without changing behaviour

- **By reference:** the map is keyed by idempotency key, not by reference; lookup by reference is a scan, or key = reference as in practice. Per transfer:
  - `transfer_id`;
  - the request fields (amount, currency, IBAN, name, remittance, reference);
  - the outcome (`executed`, or `failed` with its reason);
  - whether it is set to report twice.
- **Per key:** whether the key was answered 503 (`errored`) and whether it was refused (`refused`, with the request).
- **Pending:** how many results are still pending (`pending.size`).

**Not possible without adding state:**
- a request count per reference (demo.sh S10 needs it);
- delivery status and attempts;
- the provider event IDs sent;
- timestamps.

Adding counters changes what is stored, not the answers `POST /transfers` gives.

**Restarts:** state is lost on restart (`app.ts:21`, a known gap). An endpoint would answer "no such transfer" after a restart, which is why demo.sh also checks the container ID (S11).

---

## 5. Other things that look ad hoc or make tests harder

1. **No health endpoint (README, Known gaps).** Readiness is probed by fetching a seed account's balance: `wait-for-stack.sh:26`, `demo.sh:193`, `shared-consumer.sh:65`, `differential.sh:168, 224`. Every script hard-codes alice's TypeID for it. The workers have no HTTP surface at all (S7).
2. **The incoming-payment webhook returns nothing by design**, so the only link from a notification to its deposit is `network_transaction_id`. Neither today's API nor the planned shape looks a deposit up by it. This one gap is behind S12, S13, and the 20 Node and 21 Go `depositByNetworkId` call sites.
3. **Deposit statuses expose the internal pipeline.** The API returns `created`, `received`, `under_review`, `approved` and `rejected` as well as the final `credited` and `returned`. Withdrawal statuses are `held`, `submitted`, `executed` and `failed`. A client sees `approved` (decided, not yet posted) as distinct from `credited`.
4. **`include` takes exactly one value, `history`.** Any other value, an empty value, or `history` given twice is a 400 (`query.go:136-148`; `differential.sh:271` checks these). An empty `before=` is a 400 too, while `limit=` is read as 0 and fails as "too small", which is a different error for the same shape of input.
5. **Stale comment:** `differential.sh:200-201` says withdrawal IDs are read "Not from provider-sim's log, as demo.sh finds them". demo.sh takes withdrawal IDs from the POST response (`demo.sh:276, 303, 310, 333`); it reads provider-sim's log only to wait for a transfer and count calls (S9, S10).
6. **The demo's header comments describe the current paths.** `demo.sh:6-7` says steps 1-4 check "through the API's transactions list".
7. **Uses of the two changing endpoints:**
   - `/transactions` appears in 15 files: scripts ×3, `node/src/api/accounts.ts`, Go `accounts.go`, `handler.go`, `accounts_test.go`, Node `accounts.test.ts`, `not-found.test.ts`, the contract, README, `docs/development.md`, and the slice 1, slice 3 and test-map docs.
   - `/withdrawals/{id}` appears in 17 code and script call sites, plus the `location` header and the contract.
   - `docs/guarantees.md` and `docs/slices/slice-3-test-map.md` name test cases, so renamed tests also touch those docs.
8. **Node's e2e tests read state through data-access functions** (`getWithdrawal(pool, …)`, `balancesOf`) even though the API app is in-process and listening (`e2e/submitter.e2e.ts:31`). So today the e2e tests exercise the client API only for the POST.
9. **Go has no process-level (e2e) tests.** Real kills on Go are shown only by the demo (`docs/guarantees.md`, "Crash safety"). Go's provider client tests use an `httptest` fake, not provider-sim (`provider_test.go:22-27`). Only Node's tests and the demo depend on provider-sim's logs.
10. **`fund()` in demo.sh** posts deposits of 2000 until carol has 10000 available, re-listing 200 deposits every second for each one (`demo.sh:147-166`). The pseudo review rejects at random, so the number of rounds varies.
11. **Two problem-type namings for 400s on webhooks.** The webhooks use `invalid-notification` and `invalid-transfer-update`; the client API uses `invalid-request`. That's consistent per caller, but a third 400 type would follow neither pattern.
12. **Contract version.** `info.version` is still `0.1.0` after slices 2 and 3 added operations.
