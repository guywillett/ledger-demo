# Slice 4 — A rounded client API

Status: done; every item under *Done when* is met, except the tag, which follows the review and merge
Implementation: Node and Go together, step by step, from `v0.3-go`
Builds on: slices 1–3 and [ADR 0002](../decisions/0002-deposits-and-withdrawals.md). Their decisions hold unless this spec says otherwise.

**Scope principle:** complete the resources the API already has; add no new capabilities (decision 63). The API is a generic payment ledger's, not a model of one product's requirements.

## Goal

Today the client API has four operations: the balance, a list called `transactions` that holds only deposits, creating a withdrawal, and reading one withdrawal. Deposits can be listed but not fetched; withdrawals can be fetched but not listed; the account itself can't be read. Tests and scripts work around the gaps: they read the database, grep logs, list 200 deposits to find one, and re-encode UUIDs as TypeIDs in awk.

After this slice:

1. **Every client resource is complete and consistent:** the account, its balance, its deposits and its withdrawals, each readable singly and as a list where that makes sense, with one list shape, one validation behaviour and one set of field conventions; plus the account's transactions, deposits and withdrawals together.
2. **The workarounds are gone or justified.** Each one in the [inventory](slice-4-inventory.md) (a snapshot at `v0.3-go`, summarised under *Done when*) is either replaced by the client API (or provider-sim's test-only endpoint) or recorded as legitimately outside the client API.
3. **A feature built in both implementations at once.** The first change since the port: designed once, in the contract; implemented in Node and Go in each step; equivalence checked as in slice 3 (vectors, contract, demo on every stack, the differential check).

## In scope

- The client API under `/accounts/{account_id}` (decision 64): the account, its balance, deposits (list and single), withdrawals (list, single, create), transactions (deposits and withdrawals together)
- List filters: `status` on deposits and withdrawals; `network_transaction_id` on deposits
- `GET /health`
- The consistency rules (decisions 66–70) applied to every client operation, old and new
- Internal reasons kept out of the client API (decision 76): a fix to what `include=history` exposes today
- A test-only read endpoint on provider-sim (decision 73)
- Scripts, e2e tests and integration tests moved off their workarounds; README, development docs, `docs/guarantees.md`

## Out of scope

- **A statement** (booked movements over a period). A new capability, not a gap in existing resources: a later slice if at all. Open questions, recorded so the thinking isn't lost:
  - which movements a client sees, and how they're labelled (bucket names are internal);
  - a running balance: none is stored, and neither `id` nor `created_at` order is guaranteed to match the order in which concurrent transactions changed the balance row, so a computed "balance after" could show values that never existed. It would need a stored balance per movement, or a defined order (e.g. a per-account sequence assigned under the balance row's lock);
  - no index gives an account's movements in order (`balance_movements_account_idx` is on `account_id` alone);
  - periods and boundaries (calendar months, time zones), opening and closing balances;
  - export formats (CSV, PDF, CAMT.053), generated on demand or in the background.
- Authentication and authorisation (designed for, not built: decision 64)
- Client-facing deposit statuses (a mapping at the boundary, e.g. pending/credited/returned): statuses stay as stored
- Cancelling a withdrawal, client webhooks, search, richer filters, account creation or listing, cross-account lists
- Health endpoints for the workers (they have no HTTP surface)
- Schema changes beyond indexes the new reads need (if any; raised as a finding first)

## The client API after this slice

| Operation | Notes |
|---|---|
| `GET /health` | Outside the account scope. Readiness: 200 `{"status":"ok"}` if the database answers, else 503 (decision 72) |
| `GET /accounts/{account_id}` | The account: `id`, `name`, `iban`, `created_at` (decision 71) |
| `GET /accounts/{account_id}/balance` | As today, plus `account_id` |
| `GET /accounts/{account_id}/deposits` | List (decision 66); filters `status`, `network_transaction_id`; `include=history` |
| `GET /accounts/{account_id}/deposits/{deposit_id}` | `include=history` |
| `POST /accounts/{account_id}/withdrawals` | As today; `Location` now `/accounts/{account_id}/withdrawals/{id}` |
| `GET /accounts/{account_id}/withdrawals` | List; filter `status`; `include=history` |
| `GET /accounts/{account_id}/withdrawals/{withdrawal_id}` | `include=history` |
| `GET /accounts/{account_id}/transactions` | Deposits and withdrawals together, newest first (decision 67) |

Removed: `GET /withdrawals/{withdrawal_id}`, and `GET /accounts/{account_id}/transactions` as a list of deposits only (its name now means what it says). The provider webhooks are unchanged.

## Equivalence

As slice 3: the three levels and their evidence carry over. The differential check (slice 3 decision 61) is extended to every new read, including the transactions list's cursors used across implementations, and the filters' edge cases. New shared vectors only if a new rule decides an outcome (none expected: filters are exact matches).

## Steps

Each step: the contract change and the Node implementation in one commit (Node is the reference), then the Go implementation in a second; both reviewed together, and the step is done when both suites, the demo on every stack and the differential check pass. A step that changes or removes an endpoint also updates the scripts and tests that call it, minimally, so they keep passing; removing the workarounds is step 7's. CI may be red between a step's two commits; branches are pushed at step ends.

1. **Groundwork:** `GET /health`; validation reporting all problems at once on the existing GETs (decision 68); the contract documents what both implementations already do (`x-request-id` on every response, HEAD, unknown query parameters ignored, repeated ones a 400) and its version is bumped. The scripts' readiness probes use `/health`.
2. **The account:** `GET /accounts/{account_id}`; the balance gains `account_id`. Scripts read the seed accounts' IBANs from the API instead of hard-coding them.
3. **Deposits:** the list and the single deposit under the account, the list shape (decision 66), the filters (decision 70), the field changes (decisions 69 and 76). The old deposits-only `/transactions` goes; the scripts and tests that used it move to the deposits list (still listing and filtering until step 7).
4. **Withdrawals:** the list and the single withdrawal under the account; `Location` changes; `GET /withdrawals/{id}` goes, and its callers (the demo, `differential.sh`, tests) move to the nested path.
5. **Transactions:** the combined list (decision 67).
6. **provider-sim's test-only endpoint** (Node only; decision 73).
7. **Off the workarounds:** scripts, e2e and integration tests use the client API (or provider-sim's endpoint) wherever the inventory says one now answers, and each remaining one is recorded as legitimately outside it; the differential check covers the new reads; README, `docs/development.md`, `docs/guarantees.md`; the test map is not updated (a snapshot at `v0.3-go`). *Done when*; tag.

Steps 3 and 4 may split (list, then single) if a review gets large.

## Done when

- [x] Every operation in *The client API after this slice* is in the contract, implemented in Node and Go, and every test response is checked against the contract in both: `contracts/openapi.yaml` (0.4.0); Node `test/api/` (`health`, `accounts`, `deposits`, `withdrawals`, `transactions`), each response through `getJson` / `postWithdrawal`; Go `internal/api/` (`health_test.go`, `accounts_test.go`, `deposits_test.go`, `withdrawals_list_test.go`, `withdrawals_test.go`, `transactions_test.go`), each through `testAPI.get` / `postRaw` (`contracttest.Check`)
- [x] The consistency rules (decisions 66–70) hold for every client operation, old and new: each operation's tests above (the list shape and its cursors, another account's resource as a 404, "reports every problem at once", the filters), and `scripts/differential.sh` both ways (every page with cursors taken from either implementation, the filters' and parameters' edge cases, byte for byte)
- [x] No internal reason reaches the client API (decision 76): a rejected or returned deposit shows only the neutral reason in its body, and none in its history: `deposits.test.ts` (`includes the status history with include=history, with no reasons`, `uses "returned to the sender" as the neutral text`), Go `TestDepositsIncludesHistoryWithoutReasons`
- [x] Each workaround in the inventory is gone, or recorded below as legitimately outside the client API, with its reason (step 7a's three commits):
  - scripts: withdrawals read from the database and their TypeIDs computed in awk (`differential.sh`); deposits found by listing 200 and grepping (`demo.sh`, `shared-consumer.sh`); "is anything unfinished" scanned from full lists; seed IBANs hard-coded; readiness probed through alice's balance; provider-sim's transfers and request counts read from its logs
  - tests (both suites): deposits found by network ID in the database; withdrawal counts per account; deposit status in e2e; balances and withdrawal statuses read through data-access functions in API and e2e tests where the API answers
  - provider-sim's log reads in Node's e2e tests

  What stays, and why (each is something the client API deliberately doesn't show, or isn't its job; the scripts and tests say so where they do it):
  - **Internal state**, read from the database (or JetStream):
    - suspense: an unmatched deposit has no account, so it is in no account's list (the webhook tests; the demo finds it in the reconciliation report);
    - provider identifiers: `provider_account_id` and `provider_transfer_id` (decision 69);
    - the outbox and its events, and "one transaction" (`xmin`);
    - the ledger's movements (`hold`, `execute`, `book`, `credit`): there is no statement (*Out of scope*);
    - conflict records, transfer-update rows, the delivery log and the consumer's delivery count;
    - "stored nowhere at all" after a webhook's 400: a row with no account would be in no list, so the database is the only place to show its absence;
    - the schema's CHECKs, tested directly.
  - **Logs of internal behaviour:** the API's own log lines, where the log is the behaviour under test; the workers' lines in the e2e tests and the shared-consumer check (started, "deposit processed" by implementation, "event published", "shutting down"); the submitter's delivery attempt on a redelivery (`demo.sh` step 13) and its "provider outcome unknown; will retry" (e2e).
  - **Reconciliation:** the scripts' and the e2e tests' final checks, which also cover the outbox and the books (Go's `expectConsistent` stands in for it, decision 50). `demo.sh` now waits for its deposits through the status filter, and reconciles once at the end.
  - **Direct set-up:** accounts, funding, deposits and withdrawals created by SQL or the ledger's functions (there is no account creation, and tests need states the API can't produce on demand); the seed accounts' IDs as constants (there is no account list).
  - **provider-sim's container ID** (`demo.sh` step 13): its state is in memory, so the request count means "one call" only if the same container answered throughout.
- [x] Node's e2e tests read state through the client API where it answers: `e2e/deposit-worker.e2e.ts` and `e2e/submitter.e2e.ts` (deposits, withdrawals and balances through the API, provider-sim's transfers through its test-only endpoint)
- [x] The demo passes on every stack in CI; the differential check passes, covering every new read: CI on `d2c60a9` (the four demo legs, `shared-consumer.sh`, `differential.sh`); locally, at step 7, also `differential.sh` the other way (Go API against Node; 2,460 requests, all the same)
- [x] README, `docs/development.md` and `docs/guarantees.md` describe the new API (step 7b; every test and file `guarantees.md` cites, and every relative link in the four documents, checked to resolve)
- [x] Tag `v0.4-api` on the merge commit (after the review and merge)

## Known gaps (deliberate)

All earlier gaps apply. In addition:

- **No versioning.** The break is clean (decision 65); a deployed API with clients would need versions or a deprecation period.
- **The cursor is a TypeID, not an opaque token** (decision 66): clients can see and depend on its meaning, so changing the pagination mechanism later would be a breaking change.
- **Deposit statuses are the internal pipeline's** (`received`, `under_review`, `approved`, …); a client-facing mapping is out of scope.
- **`network_transaction_id` does three jobs that real payments keep apart.** It is our idempotency key for the provider's notifications (`UNIQUE (network, network_transaction_id)`, slice 1), the identifier for tracing the payment with the provider, and, from this slice, the payment's reference for payer and payee (decision 69). In reality these are different identifiers with different properties:
  - deduplicating notifications: the provider's notification or transaction ID;
  - tracing: the interbank transaction ID;
  - the parties' reference: the payer's end-to-end reference and remittance text. The payer sets these, so they aren't unique (reused, or `NOTPROVIDED`) and could never be an idempotency key; they're what clients mostly search and reconcile by.

  Slice 1 already notes that a provider may expose several IDs for one transaction (*Data model*, its `provider_transaction_refs` evolution). A deposit also carries no other payer-side details (payer name and IBAN). Separating them is a schema change: the compression is a side effect of keeping the model small.
- **Provider payloads aren't kept raw:** the webhooks store the fields they map (incoming payments into `deposits`, transfer updates into `transfer_updates`) and accept, then drop, the rest (the tolerant reader). Real integrations keep the provider's raw body beside the parsed record: an audit trail of exactly what the provider said, and the means to backfill a field mapped later (a provider's undocumented or unmapped data is a common source of operational gaps: questions from customers the stored record can't answer). A raw body holds personal data (IBANs, names), so it needs its own retention rule, separate from the parsed record's.
- **No client-safe reason codes:** a rejected or returned deposit's client-facing reason is one fixed text (decision 76). A real system would give a scheme reason code where one applies (SEPA's return reason codes, e.g. `AC04` account closed, `MS02` reason not specified), still never the compliance reason.
- **The transactions list returns full representations.** A real activity feed would be a compact summary per item (direction, display amount, counterparty, date), designed with the app's UX, with the full details behind the item. It would need payer details and client-facing statuses, both out of scope (decision 67). A placeholder in their place (a generic payer name, say) would present a gap as data, so it isn't faked here either.
- **Filters are minimal:** one `status` per request, exact `network_transaction_id`; no date ranges, amounts or text search.
- **No statement** (see *Out of scope* for its open questions).
- **provider-sim's state is in memory:** its test-only endpoint answers "no such transfer" after a restart (the demo checks the container is the same).

## Decisions

63. **Complete the existing resources; add no new capabilities.** The scope test for every endpoint, field and filter: does it complete a resource the API already has, or remove a workaround the inventory found? A statement, cancellation, search and client webhooks fail it. The API is a generic ledger's, not one product's.
64. **Every client resource lives under `/accounts/{account_id}`, and the account in the path is a scope.** Deposits and withdrawals belong to exactly one account and never move, so the hierarchy is real. Every read of a resource takes the account (`WHERE id = $1 AND account_id = $2`): a resource of another account is 404, the same answer as one that doesn't exist, decided by the database in the same query rather than by reading the resource and checking its owner (slice 1 decision 8, applied to ownership). Reads of a resource's own rows (its history) take the IDs an account-scoped read returned: history rows have no account column. Handlers are written as if authorisation already existed, so adding it later is one check at one path prefix ("may this principal act on account `a`?"), not a check per handler: the per-handler check is where "fetch someone else's object by id" bugs come from. Considered: shallow nesting (collections under the account, single resources at the top level by global ID, as today's withdrawals and as Stripe does). It's a sound convention, but it needs each single-resource handler to find and check its resource's account. The provider webhooks and `/health` stay outside the scope: they aren't on behalf of an account.

    Not found: a single resource answers its own not-found problem (`deposit-not-found`, `withdrawal-not-found`) whether or not the account exists, from the one account-scoped query. A list answers `account-not-found` for an unknown account (it checks the account, as `/transactions` does today), so an empty list means an account with nothing in it.
65. **A clean break, not a deprecation.** `GET /withdrawals/{id}` and the deposits-only `/transactions` change without aliases: the API is at `v0.x` with no clients but our own tests and scripts. Recorded as a known gap: a deployed API would version or deprecate. The contract's `info.version` goes from `0.1.0` to `0.4.0`, matching the tag.
66. **One list shape.** Every list answers `{"data": [...], "next_before": <id> | null}`: `data` for every list (Stripe's convention; generic client code works on any list), `null` marking the last page (no `has_more`, which would say the same). The cursor is the last item's TypeID, passed back as `before`: keyset pagination by object ID, as Stripe's `starting_after`. A `before` naming another account's resource, or none, is a position in time, not a reference: accepted, documented, not looked up (rejecting it would cost a query to refuse something harmless). `limit` keeps today's rules (1–200, default 50, and its grammar; slice 3 decision 54).
67. **`/transactions` is the account's deposits and withdrawals together, newest first.** A client showing activity would otherwise merge two paginated lists itself, two cursors interleaved, and need both calls to succeed. Both tables' IDs are UUIDv7, ordered by creation time across tables, so the list is one `UNION ALL` of the two account-scoped queries, each `WHERE id < $before`, ordered by `id DESC` with one limit; both indexes exist. The union decides the page (its items, order and cursor) on IDs alone; each item's full row is then read by ID within the account, one statement per kind, and mapped as its own endpoint maps it, so the list's items are its endpoints' representations by construction; a row missing from the read by ID is an invariant violation (a 500), not a shorter page. Each item is the full deposit or withdrawal, as its own endpoint returns it, with a `type` field (`deposit` or `withdrawal`). `type` is on every deposit and withdrawal representation, not only in this list (Stripe's `object` field is the precedent): self-describing everywhere, and one representation per resource. No `include=history` here (two history shapes in one list), no filters (decision 63: the per-resource lists have them). The order is creation order, not settlement order: a deposit credited after a later withdrawal still sorts by when it arrived. The cursor is a `dep_` or `wd_` ID.
68. **Validation reports every problem at once, on every operation.** The POST already does (path, header and body); the GETs answer an invalid path parameter alone, then the query. They change to report all of them, as one `invalid-request` problem with `errors` in a defined order (path, then query, each in the contract's parameter order). A client fixes everything in one round trip, and the rule is one rule.
69. **One set of field conventions for deposits and withdrawals:**
    - `failure_reason` at the top level of both, `null` unless the deposit was rejected or returned or the withdrawal failed. A withdrawal's is the provider's reason, as today; a deposit's is a fixed, non-disclosing value, the same for both statuses (the deposit table has no reason column), never the internal reason (decision 76).
    - Provider identifiers stay at the edge, except the payment's reference: `provider_account_id` leaves the deposit, as `provider_transfer_id` never appeared on the withdrawal. `network_transaction_id` stays, as the payment's reference: the one identifier both payer and payee can know (a payer's bank can give it as proof of payment, and support or the client needs to go from it to the deposit). It compresses several identifiers that real payments keep apart (*Known gaps*): here it stands in for the payer's end-to-end reference, which a real deposit would carry separately. Considered: hiding it as a provider identifier. A human quoting a deposit to support would use its `dep_` ID, but a disputed or unexplained payment starts from the payer's side, with no `dep_` ID to quote.
    - `idempotency_key` stays on the withdrawal: it's the client's own key, returned to the client.
    - Statuses as stored, for both (see *Out of scope*).
70. **Filters are exact matches, one value each.** `status` on deposits and withdrawals, one of the resource's statuses that an account's list can hold (another value is a 400 naming it, `unmatched` included: an unmatched deposit has no account, so the filter could only ever answer empty; a repeated parameter is already a 400); `network_transaction_id` on deposits (the payment's reference, decision 69; `network` is implied by the account's single scheme). Filters combine with pagination: the cursor applies within the filtered list. They ride the existing `(account_id, id DESC)` indexes; `status` is filtered row by row, acceptable at this scale (a known gap if accounts grow large).
71. **`GET /accounts/{account_id}` returns the account itself:** `id`, `name` (its unique label), `iban`, `created_at`. Not the balance (its own sub-resource, kept: one way to read each thing), not `provider_account_id` (decision 69). The table has no status or holder name; the resource is thin, and that's fine.
72. **`GET /health` is a readiness check of the API:** it runs `SELECT 1` and answers 200 `{"status":"ok"}`, or 503 with a problem body if the database doesn't answer within a short timeout. Outside `/accounts` (decision 64), HEAD as every GET. It replaces the scripts' habit of probing alice's balance. A readiness check, answered through the main pool: it reports whether this instance can serve right now, so a pool too busy to lend a connection within the timeout is not ready (pgx's acquire honours the context, so one timeout bounds the wait for a connection and the query). Considered: a dedicated one-connection pool, or a new connection per probe; both would say "ready" while real requests wait on an exhausted pool, and a connection per probe adds setup cost and can use up the database's connections. A readiness failure only takes the instance out of rotation; it must never be a liveness probe, whose failure restarts the process: a liveness check depends on nothing outside the process (no database), just answers "ok" at once, so a slow database can't get healthy processes restarted. No liveness endpoint in this slice: compose has no probes. Logged only when it fails (in both implementations): probes call it often, and a line per probe would bury the access log. The workers have no HTTP surface; their readiness stays a log line (*Out of scope*).
73. **provider-sim gets a test-only read endpoint**, `GET /_test/transfers?reference=…`: the transfer for a reference (`transfer_id`, the request's fields, its outcome, whether it reports twice) and the number of transfer requests received for it (a counter, new state: today a repeat is only logged). Test-only by name (the `/_test/` prefix), by documentation (a comment at the route and in provider-sim's module header, and in `docs/development.md`) and by rule: only tests and scripts call it; the system under test never asks the provider what happened this way (the submitter learns outcomes from its call's answer and the webhook). provider-sim is Node only (slice 3 decision 50).
74. **Each step lands in both implementations, as two commits:** the contract with Node (the reference), then Go. A step is reviewed as one and done when both pass, with the demo on every stack and the differential check.
75. **Tag `v0.4-api`** on the merge commit.
76. **Internal reasons never reach the client.** A compliance decision's reason is internal: telling a customer why a payment was rejected can itself be an offence when the suspicion is money laundering ("tipping off", under the EU anti-money-laundering directives and, in the UK, the Proceeds of Crime Act). So reasons are split by audience:
    - internal reasons stay where they are (the history tables), visible to operators through logs, reconciliation and the database, never through the client API;
    - a rejected or returned deposit's `failure_reason` is a fixed, non-disclosing value, `"returned to the sender"`, whatever its internal reason. Both statuses, with the same text: both are failures, and a client polling between the two (rejected, not yet returned) would otherwise see a failed deposit with no reason;
    - a withdrawal's failure reason comes from the provider or the scheme and is meant for the customer, so it stays;
    - history rows exposed with `include=history` carry `reason` only where it's client-facing: `null` on a deposit's compliance transitions, the provider's reason on a withdrawal's. The field stays in both shapes.

    This fixes existing behaviour: today `include=history` returns the compliance check's reason on deposits (harmless demo text, but the wrong shape). Considered: no `failure_reason` on deposits (the `returned` status says enough); simpler, but it breaks decision 69's one set of fields, and a neutral reason is what real APIs tend to give.
