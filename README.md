# ledger-demo

[![CI](https://github.com/guywillett/ledger-demo/actions/workflows/ci.yml/badge.svg)](https://github.com/guywillett/ledger-demo/actions/workflows/ci.yml)

A small, simplified payment ledger built to show one thing: **money moves exactly once,
whatever goes wrong.** Deposits are booked through a (pseudo) compliance review; client
withdrawals are held, sent to a (fake) payment provider and settled from its webhooks.
Implemented in Node.js/TypeScript; a Go implementation against the same schema, contracts and
demo is next.

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

Each of these is handled and proved. Every guarantee below names the test that shows it, and
[`scripts/demo.sh`](scripts/demo.sh) causes the failures against the running system (repeats,
conflicts, a deposit-worker killed mid-review, the submitter killed mid-transfer), then
reconciles the books and fails if they don't agree. CI runs it on every push.

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

  GET /accounts/:id/balance, /transactions, /withdrawals/:id        reconciliation: do the books agree?
```

## What it demonstrates

| Guarantee | How | Proved by |
|---|---|---|
| A repeated incoming payment notification changes nothing | `UNIQUE (network, network_transaction_id)` and one upsert statement: same details → a duplicate, counted | `test/api/incoming-payments.test.ts` ("records the same notification sent 5 times once…", "records 10 concurrent identical notifications once"); `scripts/demo.sh` steps 1–2 |
| An incoming payment notification that can't be booked is recorded, never ignored | an unknown account goes into suspense (status `unmatched`: no account, no event, no movement; a retry stays a duplicate of it, resolution is never automatic); the same network ID with other details becomes a conflict record, the deposit unchanged; both alerted and reported by reconciliation | `test/api/incoming-payments.test.ts` ("records a valid IBAN that is not ours as unmatched…", "keeps it unmatched after an account with that IBAN is created…", "records a different amount as a conflict…"); `scripts/demo.sh` steps 3–4 |
| An event is never lost between commit and publish | the event is written to `outbox` in the same transaction as the state change; a relay publishes it at least once (claims leased with `FOR UPDATE SKIP LOCKED`, marked after the stream's acknowledgement); the stream drops a repeat by its `msg_id` | `test/api/deposit-events.test.ts` and `test/api/withdrawals.test.ts` (one outbox row, same transaction); `test/outbox/outbox.test.ts` (rollback, leases, disjoint concurrent claims); `test/relay/relay.test.ts` ("publishes again after a relay crashed between publishing and marking, and the stream drops the repeat") |
| A repeated or concurrent event moves money once | every step is a conditional transition (`UPDATE … WHERE status = $expected`), never check-then-act | `test/deposit-worker/handler.test.ts` (same event twice, two consumers at once) |
| A retried withdrawal request never takes money twice | the client's `Idempotency-Key` is unique per account on the withdrawal itself, so only a request that commits takes it (a `422`, `404` or crash leaves it free); a repeat returns the withdrawal's current state, a reused key with other details is a `409` | `test/api/withdrawals.test.ts` ("answers a repeated request with the same withdrawal…", "creates one withdrawal for 10 concurrent requests with one key", "refuses insufficient funds with 422, storing nothing, and the key stays free"); `scripts/demo.sh` steps 8–9 |
| Insufficient funds is decided atomically with the hold | the hold is a `move(available → held)` in the request's own transaction, and the balance CHECK is the insufficient-funds check (no check-then-act), so concurrent withdrawals can't overspend | `test/ledger/withdrawals.test.ts` and `test/api/withdrawals.test.ts` ("lets exactly one of two concurrent withdrawals that together exceed available…"); `scripts/demo.sh` step 10 |
| An unknown outcome is retried with the same key, never guessed | the submitter sends each withdrawal with its TypeID as `Idempotency-Key`, built only from the stored withdrawal; a timeout or `5xx` leaves it `held` and asks again later; only a definitive refusal releases funds | `test/submitter/provider.test.ts` ("maps a timeout to unknown, and the next call with the same key gets the same, single transfer"); `test/submitter/handler.test.ts`; `test/e2e/submitter.e2e.ts` ("settles once after the submitter is killed mid-call…", `SIM ERROR`); `scripts/demo.sh` step 13 |
| A provider result is applied exactly once, even when it arrives first, twice, or contradicts itself | the update is recorded and the withdrawal settled in one transaction, by a conditional transition from `held` or `submitted`; a resend is counted, a second event for the same result skips, a contradiction (the other final status, another transfer ID) or an unmatched update is recorded and alerted, moving no money | `test/api/transfer-updates.test.ts`; `test/e2e/submitter.e2e.ts` (`SIM EARLY`, `SIM DUPLICATE`, `SIM FAIL`, `SIM REFUSE`); `scripts/demo.sh` steps 11–12 |
| Provider webhooks follow the webhook contract | `204` with no body for everything recorded or deliberately handled (so they are no oracle for what exists), `400` only for an unusable body, `500` when a retry helps | `test/api/incoming-payments.test.ts`, `test/api/transfer-updates.test.ts` (every outcome `204`; "answers 400 (not 415)…") |
| An answer that signals our bug moves no money | a provider `400`/`409`, a transfer-ID mismatch or a ledger invariant violation terminates the message with an alert and leaves the withdrawal `held` for a person | `test/submitter/handler.test.ts` ("treats invalid (400) as our bug…", "terminates a ledger invariant violation with an alert…") |
| Crash safety | each step commits on its own; a redelivery skips what's done, and a recorded decision is never re-made | `test/deposit-worker/handler.test.ts` (crash after booking, after deciding); `test/e2e/deposit-worker.e2e.ts` and `test/e2e/submitter.e2e.ts` (real `kill -9` mid-review, mid-call); `scripts/demo.sh` steps 5 and 13 |
| Horizontal scaling | instances share one durable JetStream consumer; each fetches only as many messages as it has free slots; relays claim disjoint batches | `test/e2e/deposit-worker.e2e.ts` (two workers; long reviews never redelivered); `test/outbox/outbox.test.ts` ("gives two concurrent claims disjoint events (SKIP LOCKED)") |
| The ledger protects itself | `move()` records each movement under `UNIQUE (reference_type, reference_id, reference_step)`; a reused reference with other details is refused; a reference's type and step are checked as a pair, in the database and in the `Movement` type | `test/ledger/move.test.ts`, `test/ledger/withdrawals.test.ts` |
| Audit trail | `balance_movements`, `deposit_status_history` and `withdrawal_status_history` are append-only, enforced by triggers | `test/ledger/move.test.ts`, `test/ledger/lifecycle.test.ts` |
| Exact money | integer minor units as `bigint` in Postgres, `BigInt` in TypeScript, digit strings in JSON ([decision 0001](docs/decisions/0001-money-representation.md)) | amounts above 2^53 in the API, ledger, database and logging tests |
| The books agree | reconciliation checks balances against deposit and withdrawal statuses and against the movement journal; reports stuck deposits, withdrawals and events, suspense and conflicts | `test/reconciliation/reconcile.test.ts`; the last test of each e2e file; `scripts/demo.sh` |
| One contract for every implementation | hand-written `contracts/openapi.yaml` and `contracts/events/`; every test response and published event is checked against them | `test/contract.ts`, `test/shared/events.test.ts` |

## Design principles

The decisions a reviewer should see first; each is argued in the [slice 1](docs/slices/slice-1-incoming-payments.md) and [slice 2](docs/slices/slice-2-outbox-and-withdrawals.md) specs, and the numbers refer to their Decisions sections.

- **The database decides, never check-then-act** (slice 1 decision 8): every state change is a conditional `UPDATE … WHERE status = $expected`, an `INSERT … ON CONFLICT`, or a CHECK (the balance CHECK *is* the insufficient-funds check), and the code acts on `rowCount` and `RETURNING`. Two racing requests or redeliveries can't both win.
- **Balances change only through `move()`** (slice 1 decision 10): each movement is recorded in an append-only journal and applied to the balance in the same transaction, under a unique reference, so replaying the journal gives the balances, and reconciliation proves it.
- **State and its event commit together** (slice 2 decision 22): the transactional outbox, so an event can't be lost between commit and publish. Delivery is at least once, so every consumer is idempotent.
- **No external call inside a transaction** (slice 2, *Submitter*): the provider and NATS are only called outside a transaction, so a slow dependency never holds locks.
- **Unknown outcomes are never guessed** (slice 2 decision 26): a timeout leaves the withdrawal held and asks the provider again with the same key.
- **Only success consumes an idempotency key** (slice 2 decision 24): failures store nothing, so a transient fault is never replayed as a permanent answer.
- **A provider answer that signals our bug moves no money** (slice 2 decision 35): alert, terminate, leave it for a person.
- **Status codes follow the caller** (slice 2 decision 38): conventional REST codes for our client; for provider webhooks, `2xx` for anything recorded, because a provider retries everything else, and non-`2xx` only when a retry can help.
- **Events carry no personal data** (slice 2, *`withdrawal.held` v1*): IDs and amounts only, no IBANs, names or remittance text; consumers read details from the database. A stream keeps messages beyond the database's control.

## Where to look

With ten minutes to spare:

| Start here | What it shows |
|---|---|
| [`node/src/ledger/move.ts`](node/src/ledger/move.ts) | The only way a balance changes: one movement per reference and step, in the caller's transaction. A repeat is a no-op; a repeat with other details is an error. |
| [`db/migrations/`](db/migrations/) | The schema does half the work: balance and status CHECKs, uniqueness as idempotency, append-only triggers. |
| [`node/src/outbox/outbox.ts`](node/src/outbox/outbox.ts), [`node/src/relay/relay.ts`](node/src/relay/relay.ts) | The transactional outbox: claim with `FOR UPDATE SKIP LOCKED` and a lease, publish outside any transaction, mark after the ack. |
| [`node/src/consumer/delivery-log.ts`](node/src/consumer/delivery-log.ts) | Redelivery for every consumer: record, then acknowledge; a message can't be settled twice. |
| [`node/src/submitter/handler.ts`](node/src/submitter/handler.ts) | Calling a provider when the outcome can be unknown: the same key on every retry, never a guess. |
| [`node/src/ingestion/transfer-updates.ts`](node/src/ingestion/transfer-updates.ts) | A provider result applied exactly once, even early, repeated or contradictory. |
| [`node/src/reconciliation/`](node/src/reconciliation/) | Do the books agree? Balances against the movement journal against deposit and withdrawal statuses. |
| [`docs/slices/`](docs/slices/) | The specs: what each slice builds, its numbered decisions and why, and the known gaps. |

## Repository layout

```text
node/             the Node.js/TypeScript implementation: API, relay, deposit-worker, submitter, provider-sim
  src/            one folder per concern (api, ledger, outbox, relay, consumer, deposit-worker, submitter, …)
  test/           integration tests against the compose services; e2e tests kill real processes
db/migrations/    plain SQL run by dbmate, shared by every implementation
contracts/        OpenAPI and event schemas; every test response and event is checked against them
nats/             JetStream stream and consumer definitions
scripts/demo.sh   the end-to-end demo; HTTP and compose only, so it runs against any implementation
docs/             slice specs with their decisions; decisions/ for cross-cutting ones
```

## Quick start

```sh
docker compose up -d --build     # Postgres, NATS, migrations, stream setup, API, relay, deposit-worker, submitter, provider-sim
scripts/demo.sh                  # deposits: duplicates, conflicts, suspense, a killed deposit-worker, two workers;
                                 # withdrawals: repeats, insufficient funds, provider failures, a killed submitter; then reconciles
curl -s localhost:3000/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/balance    # alice
```

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) does the same from a clean checkout on every push: `npm run check`, the stack built from scratch, the integration and e2e tests, then `scripts/demo.sh`.

`scripts/demo.sh` exits `0` only if every expectation holds and, once everything has finished, the books agree and nothing is stuck (deposits, withdrawals held or submitted, unpublished events). That last check covers the whole development database, so something left unfinished by an earlier run fails it too (the report names it); `docker compose down -v` gives a clean slate. The script talks to the system only over HTTP and through compose (reconciliation runs as its own compose service, reading the shared database), so it will work unchanged against the Go implementation. It takes about a minute.

## Known gaps

Deliberate, and described with their evolution path under *Known gaps* in the [slice 1](docs/slices/slice-1-incoming-payments.md) and [slice 2](docs/slices/slice-2-outbox-and-withdrawals.md) specs. The most important:

- **Security:** no HMAC signature check on the provider webhooks, no authentication on the client API, no rate limiting (beyond Fastify's 1 MiB body limit), NATS without credentials, and one database user for everything (append-only enforced by triggers rather than per-service roles). Nothing is encrypted in transit (Postgres with `sslmode=disable`, NATS without TLS) or at rest (the JetStream file store, the Postgres volume). Mitigated by design: events carry no personal data, and consumers act only on rows already in the database, through conditional transitions, so a forged event can't create a deposit or move money; at most it starts idempotent processing that would happen anyway.
- **A customer ledger, not a general ledger:** each movement moves an amount between buckets of one customer account (`external` for money entering or leaving), so the books explain customer balances but not the bank balance. There are no company-side accounts (bank, suspense, fees, treasury), and unmatched money is a deposit status rather than a booking into a suspense account. In production: cash at the bank = customer balances + suspense + treasury, with each flow kept separate behind an intake table.
- **Webhooks are the fast path, not the guarantee:** a real integration also polls the provider (status checks for withdrawals still `submitted`, discovery of incoming payments) and reconciles against the daily statement. Webhooks are processed in the request; an inbox (store the raw payload, process separately) would suit heavy processing and audit, but isn't needed for correctness.
- **No dead-letter handling:** a message that exhausts `max_deliver`, or that a consumer terminates, is only logged; reconciliation shows the deposit or withdrawal as stuck.
- **Observability:** structured JSON logs only (with `request_id`, `deposit_id`, `event_id`); no metrics, tracing or alerting. Lines that need a person carry `alert: true`, where alerting would hook in.
- **One provider, one scheme, one currency:** the webhooks speak provider-sim's format; with several providers each gets an adapter under `src/api/webhooks/<provider>/` mapping onto the same provider-neutral ingestion functions. Validation is SEPA-specific (lengths, the basic character set, partly in database CHECKs; the SEPA country list in `node/src/shared/iban.ts` is not yet verified against the EPC's), and every amount is EUR.
- **Housekeeping:** published outbox rows are kept (pruning or partitioning is the evolution); the relay polls, so an event can wait up to 1 s (`LISTEN/NOTIFY`); idempotency keys never expire (real APIs often expire them after 24 h).
- **Simplifications:** approve/reject is random; one IBAN per account; no compliance checks on withdrawals; no health endpoint.

## How this was built

With AI pair programming, on purpose and in the open. Each slice starts as a written spec:
after discussing the goals, Claude drafts it and sets out the options for each open question,
and I make the decisions. The slice is then built in small steps. Claude Code implements each
step from a brief. I review the diff myself, with Claude Code's `/code-review` for the complex
steps. A second Claude session then reviews it with me, I push back where needed and commit.
[`CLAUDE.md`](CLAUDE.md) holds the conventions they all work to. Learning idiomatic
Node/TypeScript (and next, Go) was half the point, so no step was accepted until I understood
it.

For running it locally, the tests and the tooling, see [`docs/development.md`](docs/development.md).
