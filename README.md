# ledger-demo

[![CI](https://github.com/guywillett/ledger-demo/actions/workflows/ci.yml/badge.svg)](https://github.com/guywillett/ledger-demo/actions/workflows/ci.yml)

A small, simplified payment ledger built to show one thing: **money moves exactly once,
through retries, duplicates, crashes and concurrency.** Deposits are booked through a (pseudo)
compliance review; client withdrawals are held, sent to a (fake) payment provider and settled
from its webhooks. Implemented twice, in Node.js/TypeScript and in Go, against the same schema,
contracts and demo: each service runs either implementation, in any mix.

## The problem

Every step in a payment system can fail and these failures can lead to money moving twice, or not
at all. For example:

- **A provider retries its webhook** because our `2xx` was lost: the same incoming payment
  arrives again, or arrives again with a different amount.
- **A client retries a request** after a timeout: did the first withdrawal go through?
- **A process dies between committing and publishing:** the database has the payment, and
  nothing downstream ever hears of it (it's stuck).
- **An internal message is delivered twice, or to two workers at once:** at-least-once is the best
  delivery a broker can promise: "exactly-once" is a property of the whole system.
- **A provider call times out:** the transfer may or may not have been made, and guessing
  either way is wrong.
- **A provider's results arrive early, twice, or contradict each other.**

Each of these is handled and tested. Every guarantee below names the test files that show it
(the test cases are in [`docs/guarantees.md`](docs/guarantees.md)), and
[`scripts/demo.sh`](scripts/demo.sh) causes the failures against the running system (repeats,
conflicts, a deposit-worker killed mid-review, the submitter killed mid-transfer), then
reconciles the books and fails if they don't agree.

A few terms: *suspense* is money received for no account of ours, recorded and left for a person
to resolve; a *TypeID* is an ID with its type as a prefix (`acct_…`, `dep_…`, `wd_…`); a *slice*
is one stage of the project, with a spec of what it builds and why; and the fake provider fails
on demand, by triggers in a withdrawal's remittance text (`SIM FAIL`, `SIM EARLY`, …).

```text
Deposits
provider ──POST /provider/incoming-payments──► API ──► Postgres, one transaction:
(webhook)                              deposit `created` (no balance change)
                                       + deposit.created in the outbox
                                       (unknown account: recorded in suspense, no event)
                                               │
                                     relay (any number of instances): claim, publish, mark
                                               │
                                               ▼
                                 JetStream DEPOSITS: deposit.created
                                               │
                                               ▼
                                 deposit-worker (any number of instances)
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

  GET /accounts/:id and its balance, deposits, withdrawals, transactions   reconciliation: do the books agree?
```

## What it demonstrates

| Guarantee | How | Tested by (Node · Go) |
|---|---|---|
| A repeated payment notification changes nothing | a unique network ID; one upsert counts each duplicate | [`incoming-payments.test.ts`](node/test/api/incoming-payments.test.ts) · [`webhook_test.go`](go/internal/api/webhook_test.go) |
| A notification that can't be booked is recorded, never ignored | unknown accounts go to suspense; other details become a conflict record | [`incoming-payments.test.ts`](node/test/api/incoming-payments.test.ts) · [`webhook_test.go`](go/internal/api/webhook_test.go) |
| An event is never lost between commit and publish | the transactional outbox, a leasing relay, and the stream dropping repeats | [`outbox.test.ts`](node/test/outbox/outbox.test.ts), [`relay.test.ts`](node/test/relay/relay.test.ts) · [`outbox_test.go`](go/internal/outbox/outbox_test.go), [`relay_test.go`](go/internal/relay/relay_test.go) |
| A repeated or concurrent event moves money once | every step a conditional transition the database decides, never check-then-act | [`handler.test.ts`](node/test/deposit-worker/handler.test.ts) · [`handler_test.go`](go/internal/depositworker/handler_test.go) |
| A retried withdrawal request never takes money twice | an idempotency key per account, taken only by a request that commits | [`withdrawals.test.ts`](node/test/api/withdrawals.test.ts) · [`withdrawals_test.go`](go/internal/api/withdrawals_test.go) |
| Insufficient funds is decided atomically with the hold | the balance CHECK is the check, in the request's own transaction | [`withdrawals.test.ts`](node/test/ledger/withdrawals.test.ts) · [`withdrawals_test.go`](go/internal/ledger/withdrawals_test.go) |
| An unknown outcome is retried with the same key, never guessed | a timeout leaves the withdrawal held; the retry reuses its key | [`provider.test.ts`](node/test/submitter/provider.test.ts) · [`provider_test.go`](go/internal/provider/provider_test.go) |
| A provider result is applied once, even early, twice or contradictory | recorded and settled in one transaction, by a conditional transition | [`transfer-updates.test.ts`](node/test/api/transfer-updates.test.ts) · [`webhook_transfer_updates_test.go`](go/internal/api/webhook_transfer_updates_test.go) |
| Status codes follow the caller | REST codes for our client; `204` for anything a webhook delivered and we handled | [`incoming-payments.test.ts`](node/test/api/incoming-payments.test.ts) · [`webhook_test.go`](go/internal/api/webhook_test.go) |
| An answer that signals our bug moves no money | terminated with an alert; the withdrawal stays held for a person | [`handler.test.ts`](node/test/submitter/handler.test.ts) · [`handler_test.go`](go/internal/submitter/handler_test.go) |
| Crash safety | each step commits on its own; a redelivery skips what's done | [`deposit-worker.e2e.ts`](node/test/e2e/deposit-worker.e2e.ts) · [`handler_test.go`](go/internal/depositworker/handler_test.go) |
| Horizontal scaling | one shared durable consumer, fetching for free slots only; disjoint relay claims | [`deposit-worker.e2e.ts`](node/test/e2e/deposit-worker.e2e.ts) · [`consume_test.go`](go/internal/consumer/consume_test.go) |
| The ledger protects itself | only `move()` changes balances: one journal entry per reference and step | [`move.test.ts`](node/test/ledger/move.test.ts) · [`move_test.go`](go/internal/ledger/move_test.go) |
| Audit trail | movements and status histories are append-only, enforced by triggers | [`lifecycle.test.ts`](node/test/ledger/lifecycle.test.ts) · [`deposits_test.go`](go/internal/deposits/deposits_test.go) |
| Exact money | integer minor units, digit strings in JSON, never floats ([ADR 0001](docs/decisions/0001-money-representation.md)) | [`vectors.test.ts`](node/test/vectors.test.ts) · [`money_test.go`](go/internal/money/money_test.go) |
| The books agree | reconciliation: balances against the journal and the statuses | [`reconcile.test.ts`](node/test/reconciliation/reconcile.test.ts) · Node's reconciliation, after the demo on every stack |
| One contract for every implementation | every test response and published event checked against `contracts/` | [`contract.ts`](node/test/contract.ts) · [`contracttest.go`](go/internal/contracttest/contracttest.go) |

Each row in full, with its test cases: [`docs/guarantees.md`](docs/guarantees.md).

The principles behind them, each argued in the [slice 1](docs/slices/slice-1-incoming-payments.md) and [slice 2](docs/slices/slice-2-outbox-and-withdrawals.md) specs:

- **The database decides, never check-then-act** (slice 1, decision 8)
- **Balances change only through `move()`** (slice 1, decision 10)
- **State and its event commit together** (slice 2, decision 22)
- **Only success consumes an idempotency key** (slice 2, decision 24)
- **Unknown outcomes are never guessed** (slice 2, decision 26)
- **No external call inside a transaction** (slice 2, *Submitter*)
- **Events carry no personal data** (slice 2, *`withdrawal.held` v1*)

## Two implementations

Node (`node/`) is the reference; Go (`go/`) ports the design, not the code: the same SQL, guarantees and contracts, with Go's own structure, errors and concurrency. Both use one database, one set of streams and one contract, so each service (API, relay, deposit-worker, submitter) can run either, in any mix. Provider-sim, the fake provider, and reconciliation are Node only. Provider-sim is test infrastructure; reconciliation shares none of Go's code, so it audits Go's books independently (slice 3 decision 50).

Reading the Go: start at [`go/cmd/api/main.go`](go/cmd/api/main.go) (how a process is wired), then [`go/internal/ledger/`](go/internal/ledger/).

Equivalence is checked at three levels ([slice 3, *Equivalence*](docs/slices/slice-3-go.md#equivalence)):

- **Decisions:** every rule that decides an outcome (IBANs, amounts, idempotency) passes the same shared test vectors in both suites.
- **Contract:** everything observable matches the contract, and CI runs the demo on four stacks (all-Node, all-Go, and the two mixes that cross every boundary both ways), with a Node and a Go deposit-worker sharing one consumer.
- **Bytes:** the two APIs' responses are byte-identical, checked though not required (`scripts/differential.sh`).

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) also runs each implementation's checks and tests, from a clean checkout on every push.

They differ on purpose in two places: Go's 30 s request deadline, and its cancelling of unfinished handlers at shutdown (slice 3 decisions 58 and 59). Both are released together; deployed separately, they'd need versioned contracts.

## Quick start

All Node by default; the variables below choose Go per service.

```sh
docker compose up -d --build     # Postgres, NATS, migrations, stream setup, API, relay, deposit-worker, submitter, provider-sim
scripts/demo.sh                  # deposits: duplicates, conflicts, suspense, a killed deposit-worker, two workers;
                                 # withdrawals: repeats, insufficient funds, provider failures, a killed submitter; then reconciles
curl -s 'localhost:3000/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/transactions?limit=5'   # alice's latest
```

`scripts/demo.sh` takes about a minute and exits `0` only if every expectation holds and the books agree ([details](docs/development.md#the-demo)).

Either implementation, per service: set `API_IMPL`, `RELAY_IMPL`, `DEPOSIT_WORKER_IMPL` or `SUBMITTER_IMPL` to `go` (anything unset runs Node). Export them, or put them in `.env` (git-ignored), so every compose command, the demo's included, sees the same choice. A mixed stack:

```sh
export API_IMPL=go RELAY_IMPL=go     # Go API and relay; Node deposit-worker and submitter
docker compose up -d --build && scripts/demo.sh
```

Every log line names its implementation (`"implementation":"go"` or `"node"`), so a mixed stack's logs say who did what.

## Where to look

With ten minutes to spare, start with these. Each concept is in both implementations, and both are worth reading: the same design, in each language's idiom.

| Start here | What it shows |
|---|---|
| [`node/src/ledger/move.ts`](node/src/ledger/move.ts) · [`go/internal/ledger/move.go`](go/internal/ledger/move.go) | The only way a balance changes: one movement per reference and step, in the caller's transaction. A repeat is a no-op; a repeat with other details is an error. |
| [`db/migrations/`](db/migrations/) | The schema does half the work: balance and status CHECKs, uniqueness as idempotency, append-only triggers. |
| [`node/src/outbox/outbox.ts`](node/src/outbox/outbox.ts), [`relay.ts`](node/src/relay/relay.ts) · [`go/internal/outbox/`](go/internal/outbox/), [`relay/`](go/internal/relay/) | The transactional outbox: claim with `FOR UPDATE SKIP LOCKED` and a lease, publish outside any transaction, mark after the ack. |
| [`node/src/consumer/delivery-log.ts`](node/src/consumer/delivery-log.ts) · [`go/internal/consumer/lifecycle.go`](go/internal/consumer/lifecycle.go) | Redelivery for every consumer: record, then acknowledge; a message can't be settled twice. |
| [`node/src/submitter/handler.ts`](node/src/submitter/handler.ts) · [`go/internal/submitter/handler.go`](go/internal/submitter/handler.go) | Calling a provider when the outcome can be unknown: the same key on every retry, never a guess. |
| [`node/src/ingestion/transfer-updates.ts`](node/src/ingestion/transfer-updates.ts) · [`go/internal/ingestion/transfer_updates.go`](go/internal/ingestion/transfer_updates.go) | A provider result applied exactly once, even early, repeated or contradictory. |
| [`node/src/reconciliation/`](node/src/reconciliation/) (Node only) | Do the books agree? Balances against the movement journal against deposit and withdrawal statuses, whichever implementation wrote them. |
| [`docs/slices/`](docs/slices/) | The specs: what each slice builds, its numbered decisions and why, and the known gaps. |

### Repository layout

```text
node/             the Node.js/TypeScript implementation (the reference): API, relay, deposit-worker, submitter,
                  and the Node-only provider-sim and reconciliation
  src/            one folder per concern (api, ledger, outbox, relay, consumer, deposit-worker, submitter, …)
  test/           integration tests against the compose services; e2e tests kill real processes
go/               the Go implementation: API, relay, deposit-worker, submitter
  cmd/            one binary per process
  internal/       packages by concern, layered (ledger, outbox, consumer, …), each with its tests
db/migrations/    plain SQL run by dbmate, shared by both implementations
contracts/        OpenAPI and event schemas, and the shared test vectors (vectors/)
nats/             JetStream stream and consumer definitions
scripts/          the end-to-end demo (HTTP and compose only, so it runs against any stack), the
                  shared-consumer and differential checks
docs/             slice specs with their decisions; decisions/ for cross-cutting ones
```

## Known gaps

A selection, all deliberate. The full lists, each gap with its evolution path, are under *Known gaps* in the [slice 1](docs/slices/slice-1-incoming-payments.md), [slice 2](docs/slices/slice-2-outbox-and-withdrawals.md), [slice 3](docs/slices/slice-3-go.md) and [slice 4](docs/slices/slice-4-api.md) specs. Where the project could go next: [`docs/next-steps.md`](docs/next-steps.md).

- **Security:** no webhook signatures, client authentication, rate limiting, NATS credentials, per-service database roles or encryption. By design, events carry no personal data, and consumers act only on rows already in the database through conditional transitions, so a forged event can't create a deposit or move money.
- **A customer ledger, not a general ledger:** the books explain customer balances, not the bank's; there are no company-side accounts. In production: cash at the bank = customer balances + suspense + treasury.
- **Webhooks are the fast path, not the guarantee:** a real integration also polls the provider and reconciles against the daily statement.
- **No dead-letter handling:** a message that exhausts `max_deliver` or is terminated is only logged; reconciliation shows it as stuck.
- **Observability:** structured JSON logs only, with `alert: true` where alerting would hook in; no metrics or tracing.
- **One provider, one scheme, one currency:** SEPA in EUR only; the SEPA country list isn't yet verified against the EPC's.
- **Housekeeping:** published outbox rows and idempotency keys are kept for ever; the relay polls (up to 1 s).
- **Simplifications:** random approve/reject; one IBAN per account; no compliance checks on withdrawals.

## How this was built

With AI pair programming, on purpose and in the open. The decisions and the reviews are mine;
Claude writes 90%+ of the text and code. For each slice, Claude drafts the spec from our
discussions, setting out the options for each open question, and I decide. Claude Code then
builds it in small steps, each from a brief; I review every step (with Claude Code's
`/code-review` on the complex ones, and a second Claude session reviewing with me), push back
where needed, and commit. [`CLAUDE.md`](CLAUDE.md) holds the conventions. Learning idiomatic
Node/TypeScript and Go was half the point, so no step was accepted until I understood it.

For running it locally, the tests and the tooling, see [`docs/development.md`](docs/development.md).
