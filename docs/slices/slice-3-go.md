# Slice 3 — Go implementation

Status: in progress
Implementation: Go, against the tag `v0.2-node-reference` (schema, contracts, NATS definitions, demo)
Go target: `v0.2` plus decision 56 (IBAN and SEPA text normalisation defined over ASCII, changed in Node too).
Builds on: [slice 1](slice-1-incoming-payments.md), [slice 2](slice-2-outbox-and-withdrawals.md) and [ADR 0002](../decisions/0002-deposits-and-withdrawals.md) (vocabulary). Their decisions hold for Go unless this spec says otherwise.

**Scope principle:** port the design, not the code (decision 39). The behaviour, the SQL and the guarantees carry over unchanged; the structure, error handling and concurrency are written the way Go does them.

## Goal

A second implementation of the same system, against `v0.2`'s schema, contracts, NATS definitions and demo, that can run alone or mixed with the Node one, service by service. It proves two things:

1. **The contracts are language-neutral.** Either implementation of any service can be swapped in without the others noticing, including a Node and a Go deposit-worker sharing one durable consumer at the same time.
2. **The guarantees come from the design, not from Node.** Conditional transitions, the outbox, idempotency keys and the ledger's own constraints give the same results from a second codebase, checked by the same demo and the same (Node) reconciliation.

And, as with Node: learning idiomatic Go is half the point.

## In scope

- Go implementations of the API (client API and provider webhooks), relay, deposit-worker and submitter
- Shared test vectors: the rules that decide outcomes (normalisation, parsing, comparisons), as language-neutral fixtures read by both test suites
- Selecting the implementation per service in compose; an `implementation` field in every log line (both implementations)
- The demo against a Go stack, a mixed stack, and a Node and a Go deposit-worker sharing one consumer; CI for all three
- A Go section in CLAUDE.md; README and development docs

## Out of scope

- Porting provider-sim (test infrastructure, slice 2 decision 31) and reconciliation (kept as an independent check, decision 50)
- Go process-level e2e tests: the demo kills processes through compose for both implementations (see *Known gaps*)
- Any change to the schema, contracts or NATS definitions (decision 41)
- Everything slices 1 and 2 left out (polling, authentication, double-entry, …)

## Ownership: what Go uses but doesn't own

| Thing | Owner | Go's part |
|---|---|---|
| Schema | `db/migrations/` (dbmate) | connects; never migrates. No embedded migrations (decision 40) |
| Streams, consumers | `nats/` (applied by `nats-setup`) | binds to the existing durable consumer by name; never creates or updates it (decision 40) |
| HTTP API, events | `contracts/` | implements them; every Go API test response and published event is checked against them |
| Decision rules | `contracts/vectors/` (new) | passes every vector, as Node does (decision 43) |
| Acceptance | `scripts/demo.sh` | runs unchanged against Go and mixed stacks |
| Books | Node reconciliation | none: it checks Go's results independently |

## Equivalence

A service is swappable when nothing observable distinguishes the implementations except their logs' `implementation` field (decision 42):

- **Same:** routes, status codes, problem `type` and `title`, JSON fields and formats, headers (`Location`, `x-request-id`), event subjects, payloads and message IDs (`<TypeID>.<event>`), database writes (statuses, history rows, movements, outbox rows, delivery-log rows and their `consumer` values), environment variables and their defaults, ports, log field names.
- **May differ:** problem `detail` text; log message text; the text of a library's error stored in `outbox.last_error` (a failed publish is `timeout` from nats.js, `context deadline exceeded` from nats.go; whether there is one, and when, is the same); internal timings within the configured limits.
- **Wire details fixed by the Node implementation:** amounts are digit strings (ADR 0001; a `Money` type with its own JSON methods); timestamps in JSON have millisecond precision, as JavaScript's `Date` (decision 44).

## Shared test vectors (`contracts/vectors/`)

Schemas and contracts can't catch two implementations that are both valid and still decide differently. In a mixed stack, a retry can reach the other implementation, so any rule that compares or normalises data must be identical. Each vector file is a JSON list of inputs with the expected result:

- `iban.json`: normalisation (spaces, case) and validation (checksum, length, SEPA countries), with the error for each invalid input
- `money.json`: digit-string parsing (leading zeros, zero, the maximum, signs, decimals, non-digits)
- `sepa-text.json`: the SEPA basic character set and field lengths (slice 2 decision 32)
- `typeid.json`: TypeID parsing per prefix (wrong prefix, wrong version, malformed)
- `withdrawal-same-details.json`: which pairs of requests with one `Idempotency-Key` are the same request (repeat) and which are a reuse (`409`)
- `deposit-same-details.json`: which repeated notifications are duplicates and which are conflicts

Vectors are extracted from the Node tests' existing cases; Node's tests then read them (no Node behaviour change), and Go's must pass them.

## Go conventions (summary; CLAUDE.md has the full list)

- Module in `go/` (`github.com/guywillett/ledger-demo/go`); one binary per process in `cmd/<process>/`; packages by concern in `internal/` (`ledger`, `outbox`, `relay`, `consumer`, `deposits`, `withdrawals`, `ingestion`, `api`, `money`, `ids`, `db`, …). No `shared`, `util` or `common` packages.
- Standard library first (decision 46): `net/http` (method and path patterns, no framework), `encoding/json`, `log/slog`, `context`, `testing`. Dependencies only where the standard library has no equivalent: `pgx` v5, nats.go's `jetstream` package, `google/uuid` (v7), `typeid-go`, a contract validator for tests.
- SQL: raw, parameterised, hand-mapped with `pgx` (decision 48); confined to data-access functions; no ORM, query builder or code generation.
- Errors are values: `error` returns, wrapped with `%w`, matched with `errors.Is`/`errors.As`; panics only for programmer errors at start-up. A transaction helper rolls back on any returned error (`defer`).
- `context.Context` as the first parameter of anything that does I/O; cancellation drives shutdown.
- Validation at every boundary, hand-written: decode into validated types (decision 47).
- **Any choice that isn't the usual Go way is documented where it's made, with its reason** (a comment, plus a decision here if it's structural) (decision 54).

## Steps

One commit per step on `slice-3`, reviewed before the next.

1. **Scaffold:** the module with a pinned toolchain (`go`/`toolchain` directives), `gofmt`, `go vet` and `golangci-lint` (pinned), the `config`, `db` (pool, transaction helper), `ids`, `money` and logging packages, a Go test database `ledger_go_test` (migrated by compose like `ledger_test`), a Go CI job (format, vet, lint, vulnerability scan with `govulncheck`, test), and the Go section in CLAUDE.md.
2. **Shared test vectors:** `contracts/vectors/` extracted from the Node tests; Node's tests read them; Go's `ids`, `money`, IBAN and SEPA-text code passes them.
3. **Ledger** (split in two):
   - 3a. **Ledger core and deposit postings:** `move()` and `moveOnce` with their errors, balances, invariants, deposit transitions as a closed set, and the book, credit and return postings, with the cases of `node/test/ledger/` for them (decision 57).
   - 3b. **Withdrawal postings:** hold, `markSubmitted`, execute, release, insufficient funds and the transfer-ID checks, with the rest of `node/test/ledger/` (including the movements through `held`).
4. **Read API and the deposit webhook** (split in two):
   - 4a. **HTTP foundation and read endpoints:** a multi-stage Dockerfile (one image, every binary; the first binary is the API), `cmd/api` with graceful shutdown, the HTTP server (request IDs, access log, panic recovery, problem details, unknown routes), `GET /accounts/:id/balance` and `/transactions`. Contract-checked tests.
   - 4b. **The deposit webhook:** `POST /provider/incoming-payments`: the first request body, the webhook error contract, ingestion (deposit and outbox row in one transaction; suspense; conflicts), and the duplicate-or-conflict rule passing `contracts/vectors/deposit-same-details.json`. Contract-checked tests.
5. **Relay:** claim, publish, mark; leases; the lease and batch checks against the stream's duplicate window.
6. **Consumer layer and the deposit-worker** (split in two):
   - 6a. **Consumer layer:** the delivery log (record, then settle in the order the signal-versus-record rule requires; settle once), the lifecycle generic over the event type, the pull loop (fetch only free slots), graceful shutdown, and the process runner shared by the deposit-worker and the submitter.
   - 6b. **The deposit-worker:** the handler (the deposit lifecycle, `working()` during reviews), the compliance decision, `cmd/deposit-worker`.
7. **Withdrawals** (split in three):
   - 7a. **The client endpoints:** `POST /accounts/:id/withdrawals` (`Idempotency-Key`, strict body, create and hold or classify the repeat or the reused key, `withdrawal.held` in the outbox) and `GET /withdrawals/:id`. The repeat-or-reuse rule passes `contracts/vectors/withdrawal-same-details.json`.
   - 7b. **The submitter:** the provider client, the timeout checked against `ack_wait`, the outcomes.
   - 7c. **`POST /provider/transfer-updates`:** the provider's transfer updates, on the webhook contract.
8. **Mixing:** per-service implementation selection in compose; the `implementation` log field in both implementations; demo runs against Node, Go and mixed stacks, plus a Node and a Go deposit-worker sharing the consumer; CI for all of them; README, development docs; tag.

## Done when

- [ ] Go passes every shared test vector, and so does Node
- [ ] Every Go API test response and every published event matches `contracts/`
- [ ] Go's integration tests cover the cases of the Node suites for the ledger, the API, the relay, the deposit-worker and the submitter (cases, not code)
- [ ] `scripts/demo.sh` passes unchanged against an all-Go stack, and Node reconciliation finds the books consistent
- [ ] `scripts/demo.sh` passes against a mixed stack (e.g. Go API and relay, Node deposit-worker and submitter)
- [ ] A Node and a Go deposit-worker share one durable consumer during a burst: every deposit processed once, both implementations visible in the logs, books consistent
- [ ] CI runs Go's checks and tests, and the demo against Node, Go and mixed stacks
- [ ] Every non-Go-like choice is documented with its reason (decision 54)
- [ ] README and `docs/development.md` describe running either implementation and mixing them
- [ ] Tag `v0.3-go` on the merge commit

## Known gaps (deliberate)

All of slices 1 and 2's gaps apply to Go too. In addition:

- **No Go process-level e2e tests.** Node's e2e tests start and kill real processes; Go relies on the demo (which kills the deposit-worker and the submitter through compose, whichever implementation runs them) and its integration tests. A Go e2e suite with `os/exec` would mirror Node's.
- **Provider-sim and reconciliation are Node only** (decisions 50, slice 2 decision 31).
- **No request deadline in Node.** Nothing bounds how long a Node handler runs: Fastify and the pg pool set no deadline and Postgres has no `statement_timeout`, so a query stuck on a lock holds its connection until it returns. The Go API gives each request a 30 s deadline (decision 58), so the two differ only when the database hangs that long. The relays share the gap, Node's and Go's alike: a batch's claim and mark statements have no deadline, so if the database hangs during a shutdown, the batch in progress (which shutdown lets finish) holds the process until compose's `stop_grace_period` ends in SIGKILL; its leases then expire and its events are published again, and dropped as duplicates. The evolution, for both: a per-request deadline or a `statement_timeout` on the application's connections.
- **Zod's length checks after a type error.** Zod 4 runs `.min()` and `.max()` on any value with a `.length`, even once the type check has failed: `"network_transaction_id": []` gets `Too small: expected array to have >=1 items` after `expected string, received array`, and an object with a `length` key gets `expected unknown to be …`, compared with JavaScript's loose coercion. Go reports the type error alone. Status, problem `type` and the type error are the same; only those extra entries in `errors` differ, and only for a body that is already the wrong shape. Matching them would mean reimplementing JavaScript's coercion for no caller's benefit.
- **One body limit for every route.** Both implementations accept bodies up to Fastify's default of 1 MiB on every route, far more than any client body needs (a withdrawal is a few hundred bytes): an oversized body costs parsing time and, with strict bodies (step 7), an error listing every unknown key, as large as the request. The evolution: limits per group of endpoints, at a reverse proxy in front (with its rate limiting) and again in each app (Fastify's per-route `bodyLimit`, `http.MaxBytesReader` per handler in Go), tight for the client API. Webhooks are the exception: the provider defines its payloads, and a body over the limit is refused on every retry and, when a proxy refuses it, before the app can log it. Their limit stays generous, with an alert when bodies approach it.
- **A page and its histories aren't one snapshot.** `GET /accounts/:id/transactions?include=history` reads the account, the page and the page's histories as three statements on the pool (READ COMMITTED), in both implementations, so a deposit processed between them can show a status its history doesn't reach yet (or the reverse). Minor: the next read is consistent. The evolution: one REPEATABLE READ transaction for the three reads.
- **Mixed versions over time:** both implementations are released together here. In production, implementations deployed at different times need versioned contracts and the expand–contract changes of ADR 0002.

## Decisions

39. **Port the design, not the code.** Behaviour, SQL and guarantees carry over; structure, errors and concurrency are idiomatic Go. Each step's review checks for TypeScript-shaped Go (a package per Node file, panics for errors, dependency bags).
40. **Go owns neither the schema nor the NATS definitions.** dbmate applies `db/migrations/`; `nats-setup` applies `nats/`. Go binds to the existing durable consumers by name and never creates or updates them: two implementations creating one consumer from their own settings would overwrite each other's `ack_wait` and `max_deliver`. (A standalone Go service would usually embed its migrations and create its consumers.)
41. **Nothing shared changes in this slice.** If Go needs a schema, contract or stream change, it's raised as a finding, not made in passing.
42. **Equivalence is observable behaviour** (see *Equivalence*): any service can be swapped. Problem `detail` and log message texts may differ.
43. **Rules that decide outcomes are pinned by shared test vectors** (`contracts/vectors/`), read by both test suites. In a mixed stack a retry can reach the other implementation, and two valid but different normalisations would turn a duplicate into a conflict.
44. **JSON timestamps at millisecond precision,** matching JavaScript's `Date`. Postgres stores microseconds and Go would naturally print them; both satisfy the contract, but byte-level sameness costs nothing and avoids confusing diffs.
45. **Money is `int64` in Go,** with a `Money` type whose JSON form is a digit string (ADR 0001).
46. **Standard library first.** `net/http` (Go's router handles method and path patterns), `encoding/json`, `log/slog`, `testing`. Third-party packages only where the standard library has no equivalent; each is named and pinned.
47. **Validation is hand-written:** request bodies are decoded into validated types (as Zod's output types do in Node), client bodies with `DisallowUnknownFields`, webhook bodies tolerantly (slice 2 decision 33). Struct-tag validators (`go-playground/validator`) are common in Go; the reason for not using them is recorded at step 4.
48. **Raw SQL with `pgx`, hand-written row mapping,** matching the Node conventions. `sqlc` (SQL compiled to typed Go) is the common alternative; writing the mapping teaches more, and the queries stay identical to Node's.
49. **Layout:** module in `go/` (a polyglot repo, so not at the root), `cmd/<process>/`, `internal/` packages by concern, no catch-all packages.
50. **Provider-sim and reconciliation stay Node.** Provider-sim is test infrastructure. Reconciliation run by the other implementation checks Go's books independently: an auditor that shares none of Go's code.
51. **Implementation chosen per service in compose,** so any mix runs, and an `implementation` field (`node` or `go`) in every log line of both implementations, so a mixed stack's logs say who did what. The mechanism is settled in step 8.
52. **Go tests run against the compose services,** in their own database `ledger_go_test` (the `_test` guard applies), so Node and Go suites don't share outbox rows; they share the test NATS, so the two suites aren't run at the same time locally (separate jobs in CI). The outbox and relay tests use a third database, `ledger_go_relay_test` (step 5): claims see every unpublished event in the table, and `go test` runs packages in parallel, so they mustn't see the rows other packages' tests insert. The two packages take turns on it through a Postgres advisory lock, each holding it for its whole run (`testdb.Exclusive` in their `TestMain`), as Node runs those files serially in its "relay" project.
53. **One image, one binary per process** (`cmd/api`, `cmd/relay`, `cmd/deposit-worker`, `cmd/submitter`), built in a multi-stage Dockerfile onto a minimal base image.
54. **Non-Go-like choices are documented where they're made, with their reason:** a comment at the spot, and a decision here when it shapes the structure (as 40, 44 and 47 do).
55. **Tag `v0.3-go`** on the merge commit.
56. **IBAN normalisation and SEPA text trimming are defined explicitly, over ASCII, for both implementations.** Separators are space, tab, CR, LF, no-break space (U+00A0) and narrow no-break space (U+202F), the last two because bank websites format IBANs with them. An IBAN has every separator removed and a–z upper-cased to A–Z; any other non-ASCII character is rejected, not removed. SEPA text has separators trimmed from both ends only. This changes Node's behaviour from v0.2 (an exception to decision 41, made here explicitly): inputs that relied on JavaScript's wider whitespace or full case mapping are now rejected.
57. **Go packages are layered, and the compiler enforces it** (Go forbids import cycles; Node's ledger and withdrawals modules import each other, because Node's withdrawals/ mixes entity code the ledger needs with orchestration that calls the ledger). Foundations (db, ids, money, sepa, logging) ← entities (deposits, withdrawals, outbox: types, statuses, legal transitions and their SQL) ← ledger (move, postings, balances, invariants) ← orchestration (ingestion, compliance, requests) ← processes and the API. Code that Node keeps in an entity module but that calls the ledger (withdrawals/requests.ts) lives in the orchestration layer in Go. Considered: one package for the ledger and both entities would also avoid cycles, and fewer, larger packages are common in Go. They're kept separate so the direction ledger → entities is enforced by the compiler and the layout mirrors Node's design (balances change only through the ledger's posting functions). Also considered: entities owning their postings with the ledger reduced to move() (moves the posting rules out of the ledger), and an interface the entities implement (more abstraction than two entities need). Creating a withdrawal is its first posting (insert plus hold), so the ledger owns that SQL and the withdrawals package exports no insert: a withdrawal can only be created through ledger.CreateWithdrawal, which always holds its funds. Node keeps insertWithdrawal in withdrawals/ and relies on a comment; in Go the compiler enforces it.
58. **The Go API bounds each request with a 30 s deadline** (its context; pgx cancels the query: a 500). Node has no deadline (known gap); the two differ only when the database hangs that long. An exception to decision 42, recorded here.
59. **Go consumers cancel the handlers still running when shutdown's grace period ends** (step 6a): their context is cancelled, so a query in flight is cancelled and its transaction rolls back (the state a crash leaves, reached cleanly, with the connection back in the pool before it closes), and the handler settles as a transient failure: a nak with the retry delay, the delivery recorded `failed`. Node can't cancel a promise from outside, so its unfinished handlers run on while the process exits around them, leaving an `in_progress` delivery row (the crash trace) and a redelivery after `ack_wait`. Either way the message is redelivered and handled again from committed state; Go's delivery log says `failed` where Node's says `in_progress`, and the redelivery comes after the retry delay rather than `ack_wait`. An exception to decision 42, recorded here.
60. **Amounts in requests and notifications are capped at the SEPA Credit Transfer maximum, 999,999,999.99 EUR (99,999,999,999 minor units), in both implementations.** The EPC's SCT Rulebook (EPC125-05, 2025 version 1.0, section 4.6.1, attribute AT-T002 "The amount of the SEPA Credit Transfer in euro", formerly AT-04) allows at most 999.999.999 euro and 99 euro cents. Before this, the only limit was int8's: a withdrawal of 9,223,372,036,854,775,807 overflowed `held` (SQLSTATE 22003) and answered 500 in both implementations, found while porting step 7a. The cap is a scheme rule, so it lives with the SEPA rules (Go: `sepa.MaxAmount` and `sepa.ParseAmount`; Node: `SCT_MAX_AMOUNT_MINOR` and `sepaAmountSchema` beside the IBAN and text schemas), applied to withdrawal requests and deposit notifications; `money.Amount`, balances and events keep int8's range. A larger amount is a 400 naming `amount_minor` ("is larger than the SEPA Credit Transfer maximum (999999999.99 EUR)"), never mapped to insufficient funds: an amount no transfer can have isn't a shortage. The contract gives both request fields `SepaAmountMinor` (`^[1-9][0-9]{0,10}$`, citing the rule); response amounts keep `AmountMinor`. `contracts/vectors/sepa-amount.json` pins the edges (the maximum, one minor unit more, 12 digits). An explicit exception to decision 41, as 56 was: it changes Node's behaviour from v0.2 (a notification or withdrawal above the cap was accepted, or a 500). Defence in depth, in both implementations' `move()`: an overflow on the balance update (SQLSTATE 22003), which requests can no longer cause, becomes a `BalanceOverflowError`, classified as an invariant violation, so a consumer terminates it with an alert instead of retrying it until `max_deliver`.
