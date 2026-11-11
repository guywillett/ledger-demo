# Next steps

Where the project could go after `v0.5-observability`. Not a plan: a map of the directions that
would add the most, each tied to what the system already has. The deliberate gaps they'd close are
argued under *Known gaps* in the slice specs; this page groups them by what the work would show.

## Production behaviour

How the system behaves under load and failure, beyond what the demo and CI exercise.

- **Load and soak testing** (e.g. k6): throughput and p99 latency per endpoint; contention on a
  hot account (every movement locks its balance row); connection-pool sizing; how far the
  workers scale horizontally on one durable consumer. Results kept as a short report beside the
  scenarios.
- **Concurrency and fault testing:** random interleavings of duplicates, retries and kills, with
  reconciliation as the oracle (a property-based test of the whole system); network faults with
  Toxiproxy (NATS dropping, the database slowing).
- **Observability:** built in [slice 5](slices/slice-5-observability.md) (traces through the
  outbox and NATS, the telemetry contract, logs to the backend, the transition counter, a
  dashboard); next, from its *Next*:
  - time in each status and end to end, as histograms per step;
  - the database-derived gauges (how many in each status, the oldest's age) and alerts on age, not
    count: a stuck item, a stalled pipeline, a slowing step, and the log lines that already carry
    `alert: true`;
  - exemplars, so a metric point links to a sample trace;
  - the infrastructure exporters (`postgres_exporter` with `pg_stat_statements`, NATS's);
  - message latency by stage;
  - sampling, and a real Collector or a per-node agent in production.
- **Operator tooling:** dead-letter handling and re-driving terminated messages; resolving
  deposits in suspense; housekeeping of published outbox rows (pruning or partitioning).

## Domain depth

What a real payment ledger needs that this one simplifies.

- **A double-entry general ledger:** company-side accounts (bank, suspense, fees, treasury) and
  balanced journal entries, so the books explain the bank balance, not only customer balances
  (cash at the bank = customer balances + suspense + treasury; slice 1's *Known gaps*).
- **Statements:** an account's booked movements over a period, with opening and closing balances
  and exports. The open questions, ordering under concurrency and running balances among them,
  are recorded in slice 4's *Out of scope*.
- **Provider realism:**
  - the payer's details and end-to-end reference stored apart from the network's identifier
    (slice 4's *Known gaps*: one field does three jobs);
  - raw provider payloads kept, with their own retention rule;
  - polling and reconciliation against the provider's daily statement alongside the webhooks;
  - client-safe reason codes (SEPA's return reasons);
  - client-facing statuses, and a summary activity feed designed with an app's UX.
- **Wider scope, later:** several providers behind adapters; withdrawal cancellation and limits;
  compliance checks on withdrawals; more than one currency.

## Platform and security

- **Authentication and authorisation:** every client resource lives under `/accounts/{account_id}`
  and every read is account-scoped (slice 4 decision 64), so authorisation is one check at one path
  prefix. Plus signatures (HMAC) on the provider webhooks, rate limiting, TLS everywhere, and a
  database role per service.
- **Deployment:** infrastructure as code (e.g. Terraform on AWS: managed Postgres, NATS, a
  container service), with `GET /health` as the readiness probe, and horizontal scaling shown for
  real.
- **Contract versioning,** so the two implementations could be deployed independently rather than
  released together.

## Go

- Process-level end-to-end tests in Go (real processes, killed and restarted), as Node has.
- One package with `sqlc` beside the hand-written mapping, to show the trade-off slice 3 decision
  48 records.

## Suggested order

1. Load testing, with observability's remaining metrics alongside (time in status, the gauges and
   their alerts): slice 5's traces and dashboard are what a load test reads, and the pipeline's
   metrics are best shaped against real load, where a step's p95 and an item's age mean
   something. Quick, visible, and the first questions about any system like this.
2. Authentication at the account prefix and webhook signatures.
3. The double-entry ledger: the largest step, and the one that makes this a full ledger.
4. Deployment, whenever a real environment is useful.
