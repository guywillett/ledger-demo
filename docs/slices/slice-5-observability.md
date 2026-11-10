# Slice 5 — Observability

Status: in progress
Implementation: Node and Go together, step by step, from `v0.4-api`
Builds on: slices 1–4. Their decisions hold unless this spec says otherwise.

**Scope principle:** instrument the path a payment takes and write the rules down; build what
shows the design, document the rest (decision 77). Telemetry observes the system and never takes
part in it (decisions 79 and 83).

## Goal

Today the system's only window is its structured logs. After this slice:

1. **A payment can be followed across processes.** A deposit's or withdrawal's request, the relay
   publishing its event and each consumer handling it are traces linked to one another, through the
   outbox and the stream, in either implementation and in any mix.
2. **Telemetry has a contract,** as the API and the events do: span and metric names, attributes,
   units, and three rules (names by OpenTelemetry's conventions, bounded cardinality, no personal
   data), shared by both implementations.
3. **Telemetry can't hurt the work.** With the telemetry backend stopped, the demo passes exactly as
   before.
4. **The operational design is written down:** pipeline metrics, alerts that don't depend on what
   telemetry can drop, latency by stage. Reviewers can see what's built and what would come next.

## In scope

- OpenTelemetry SDKs in both implementations (traces, metrics, logs' correlation), exporting OTLP
- A local backend, `grafana/otel-lgtm`, as one compose service (development only: decision 78)
- HTTP and database instrumentation, with the polling queries suppressed (decision 85)
- Trace context through the outbox and NATS: a carrier column, headers, span links (decisions
  80–82)
- `trace_id` and `span_id` on every log line (decision 86)
- `contracts/telemetry.md` (decision 87)
- One pipeline metric built: the transition counter (decision 88); one provisioned dashboard
- Logs also exported over OTLP, beside stdout, so Grafana shows a trace's log lines (decision 93)
- Tests of propagation, of idle silence and of privacy, in both suites; the demo with telemetry
  stopped

## Out of scope (documented, in *Next* below)

- The rest of the pipeline metrics: time per status, end-to-end duration, the database-derived
  gauges and their alerts
- Infrastructure exporters (`postgres_exporter`, NATS)
- Message latency by stage
- Sampling (none: decision 89), a real Collector, production backends
- Telemetry for provider-sim and reconciliation (test infrastructure and an auditor)

## The telemetry contract (`contracts/telemetry.md`)

Written in step 1, extended in steps 2 and 3. It lists:

- **Resource attributes:** `service.name` is the process's role (`api`, `relay`,
  `deposit-worker`, `submitter`), the same in both implementations, so a mixed stack shows one
  service per role; `implementation` (`node` or `go`), as in the logs (decision 90).
- **Spans:** name, kind and attributes of each span the system creates, by OpenTelemetry's semantic
  conventions: HTTP server spans named after the route template (`GET /accounts/{account_id}/deposits`),
  never the raw path; messaging spans with the conventions' operations (`publish`, `process`) and
  attributes (`messaging.system = nats`, the subject, `messaging.message.id`); `delivery_attempt`
  on processing spans; entity IDs (`deposit_id`, `withdrawal_id`, `event_id`) as attributes.
- **Metrics:** name, type, unit, labels, and when it's recorded.
- **The rules:**
  - **cardinality:** no unbounded value (an account, deposit, withdrawal or event ID; an amount; a
    raw path) as a metric label or in a span name; IDs go in span attributes and logs;
  - **privacy,** extending "events carry no personal data" to telemetry: database spans carry the
    statement text without parameters; no request or response bodies or headers beyond the
    conventions' safe ones; no IBANs, names or remittance text anywhere; IDs only;
  - **telemetry never touches idempotency** (decision 83).

## Equivalence

As for the API, at the level that makes sense for telemetry: the same span names, kinds, attributes
and links, and the same metric names, labels and units, in both implementations (the contract), with
the same tests in both suites asserting them. Byte-level sameness doesn't apply (IDs and timings
differ by nature). The proof that the two interoperate is a mixed stack: a Node API's request linked
from a Go relay's and a Go consumer's spans, and the reverse.

## Steps

Each step: the contract and the Node implementation in one commit, then Go in a second, as slice 4.

1. **The stack and the SDKs:** `grafana/otel-lgtm` in compose; each process starts the SDK
   (resource attributes, OTLP exporters, batching), HTTP server and database instrumentation with
   the privacy settings, the polling queries suppressed; incoming and outgoing trace context at the
   edges (decision 84); `trace_id` and `span_id` in the logs; `contracts/telemetry.md`'s first
   version. Tests: the API's spans by route template, no parameters or bodies captured, no spans
   for an idle relay or consumer, the log fields. The demo with the telemetry service stopped.
2. **Propagation through the outbox and NATS:** the migration (the carrier column); the API
   injects the context when it enqueues; the relay's batch trace and its publish spans, linked, with
   the carrier as headers; the consumers extract it and link their processing span, with
   `delivery_attempt`. Tests with in-memory exporters in both suites (the links arrive; a redelivery
   links to the same producer; an event without a carrier publishes and processes normally; a
   malformed `traceparent` changes nothing but the trace); a mixed-stack run.
3. **The transition counter and a dashboard:** `ledger_transitions_total`, recorded for applied
   transitions after the commit; one provisioned Grafana dashboard (request rates and errors by
   route, the transition counter by step, links to traces and logs).
4. **Logs to the backend too:** each process writes every log line to stdout as now and also
   exports it over OTLP (decision 93): Go through an `slog` handler that fans each record out to the
   existing JSON handler and OpenTelemetry's `otelslog` bridge; Node through pino's OpenTelemetry
   support. `OTEL_LOGS_EXPORTER` switches the copy per environment (`otlp`, or `none` for stdout
   only: decision 93). The contract's *Logs* section gains the OTLP record's mapping (severity, body,
   attributes) and the privacy rule for it. Tests: a log line in a traced context reaches an
   in-memory log exporter with its trace and span IDs; stdout's lines are unchanged; with the backend
   absent, stdout still has every line; with `OTEL_LOGS_EXPORTER=none`, nothing is exported.
5. **Docs and close:** README (a short section: how to see a payment's trace), `docs/development.md`,
   *Next* complete, *Done when*, the tag.

## Done when

- [ ] Every process in both implementations exports traces and metrics over OTLP, with the
  contract's resource attributes
- [ ] A deposit's and a withdrawal's request, the relay's publish and each consumer's processing
  are linked traces, in both implementations and on a mixed stack in both directions
- [ ] No span for an idle relay's claim or a consumer's empty fetch, and no database span for the
  polling queries
- [ ] No personal data in any span, metric or attribute (tested), and route templates in span names
- [ ] Every log line in a traced context carries `trace_id` and `span_id`
- [ ] Every log line also reaches the backend over OTLP, a request's lines shown with its trace in
  Grafana; stdout unchanged and complete with the backend down
- [ ] `contracts/telemetry.md` lists every span, attribute and metric both implementations emit,
  with the three rules
- [ ] With the telemetry service stopped, the demo passes on every stack (CI runs it so)
- [ ] `ledger_transitions_total` counts applied, committed transitions only (a duplicate delivery
  and a rolled-back transaction count nothing: tested)
- [ ] One provisioned dashboard; README and `docs/development.md` say how to use it
- [ ] Tag `v0.5-observability` on the merge commit

## Known gaps (deliberate)

All earlier gaps apply. In addition:

- **`grafana/otel-lgtm` is for development only:** one container, local storage, no high
  availability. Production: a real Collector and backends, or a managed service (e.g. AWS Distro for
  OpenTelemetry to X-Ray and CloudWatch, managed Prometheus and Grafana); the services speak OTLP, so
  that's configuration, not code.
- **Linked traces can be sampled apart.** No sampling here; with sampling, each linked trace is kept
  or dropped independently, so a link can point at nothing.
- **Telemetry can be lost under overload** (the SDKs drop rather than block) and on a crash
  (unexported data). Hence decision 88: alerting on stuck items comes from the database.
- **A rate misses what a process counts before its first export.** Each process start is a new
  series (its own `service.instance.id`), and its first sample, one export interval (10 s) after it
  starts counting, already holds everything counted so far. `rate()` and `increase()` count only
  growth between samples, and a new series has no earlier one, so those first counts are never
  seen as growth. Across a restart the total rate dips for about one rate window (the old series
  fading out, the new one ramping in) and its increase is short by the new process's first sample;
  it never drops to zero or overshoots. Negligible in steady operation; marked in the demo, whose
  restarted workers do most of their work in their first seconds. The series' totals are right;
  rates are for trends, and exact counts come from the database (decision 88). Prometheus can
  insert a zero at each series' start time (its `created-timestamp-zero-ingestion` feature, from
  OTLP's start timestamp): not enabled, as it means overriding otel-lgtm's start-up script.
- **Events enqueued before this slice have no trace context** (the column is null for them).

## Next (documented, not built)

- **Pipeline metrics.** `ledger_status_duration_seconds{entity, status}` (time in a status, observed
  when leaving it, from the history rows' timestamps, not process clocks) and
  `ledger_completion_duration_seconds{entity, outcome}` (created to final), with buckets per step
  (milliseconds for the review, seconds for a provider call, hours for a settlement). Database-derived
  gauges through `postgres_exporter`'s custom queries: `ledger_in_status{entity, status}` and
  `ledger_oldest_in_status_seconds{entity, status}`.
- **Alerts on age, not count:** a stuck item (the oldest in a status too old), a stalled pipeline
  (nothing reaching a final status while work waits), a slowing step (p95 above its usual level).
  They answer the operator's questions in order: what's stuck, where, since when, and whether it's
  ours (`under_review`) or the provider's (`submitted`).
- **Measuring change over time:** a step's p50/p95 before and after a process change, the change
  marked by a Grafana annotation; the definitions kept stable (the contract), variants as labels only
  when bounded; volume and mix confound before-and-after comparisons.
- **Technical metrics:** outbox lag (the oldest unpublished event's age), consumer lag (JetStream
  pending), delivery outcomes per consumer, provider-call latency and outcomes (unknown included),
  deposits in suspense; the SDKs' and the Collector's own dropped-export counters, alerted on.
- **Infrastructure exporters:** `postgres_exporter`; the NATS server's monitoring endpoint through
  the NATS Prometheus exporter (rates, pending, redeliveries, slow consumers; not message latency).
- **Message latency by stage (optional):** enqueue to publish; the publish acknowledgement; stored to
  delivered (from JetStream's stored-message timestamp, clamped at zero for clock skew); processing.
- **Sampling, when needed:** parent-based plus tail sampling in a real Collector, keeping every trace
  with an error, a slow one, or one touching suspense or an invariant violation.

## Decisions

77. **Build the trace path and the rules; document the operations design.** The slice builds what
    has design in it (propagation through the outbox and the stream, the contract and its rules,
    telemetry that can't hurt the work) and one pipeline metric, and documents the rest in *Next*
    with its reasoning. A reviewer sees what's done and what would come next.
78. **OpenTelemetry, OTLP, and one local container.** Both implementations use the OpenTelemetry
    SDKs (Node: `@opentelemetry/sdk-node` and its HTTP and `pg` instrumentations, the route named by
    our own Fastify hook, in the contract's `{param}` form, as Go's; Go: `go.opentelemetry.io/otel`,
    `otelhttp`, a pgx tracer), pinned, and export traces, metrics and
    logs' context over OTLP. Nothing in the code names a backend. Locally that's
    `grafana/otel-lgtm` (a Collector, Tempo, Prometheus, Loki and Grafana in one image, its tag
    pinned): less infrastructure than the application itself, for development only (*Known gaps*).
    Considered: separate Collector, Tempo, Prometheus, Loki and Grafana services (realistic, but more
    to run than the system); Jaeger for traces (traces only).
79. **Telemetry never blocks or breaks the work.** Metrics are pushed over OTLP like traces. The
    SDKs' exporters batch in the background with bounded queues and drop rather than block, so a
    slow or absent backend costs telemetry, never a request or a delivery; an exporter's failure is
    logged, not raised. Shown, not assumed: CI runs the demo with the telemetry service stopped.
    Production might add a local Collector's buffering, or scraping, to decouple further.
80. **W3C Trace Context, and span links for asynchronous work.** The propagators are W3C Trace
    Context (`traceparent`, `tracestate`) and W3C Baggage, OpenTelemetry's defaults, so a Node
    producer and a Go consumer understand each other. The relay's work and each consumer's
    processing start new traces linked to the context that caused them, not children of it
    (OpenTelemetry's messaging conventions): a request's trace ends with the request, and doesn't
    stay open while its event waits in the outbox or the stream. The relay: one trace per claim that
    returns events, a publish span per event linked to its event's originating trace. A consumer: a
    root span per message (messages are processed independently and concurrently, so a fetch isn't a
    meaningful unit), linked to the producer.
81. **The outbox stores the propagation carrier.** A new nullable `jsonb` column on `outbox`
    (proposed: `trace_context`) holds the string map the propagators inject into when the API
    enqueues an event, restored as-is when the relay publishes: whatever the propagators write is
    stored, so adding one needs no schema change. Null for events enqueued outside a traced context
    (tests) and for rows from before this slice (no backfill). The payload is untouched: trace
    context travels in headers, so payloads stay byte-identical across implementations. A
    language-neutral migration, as every schema change.
82. **The carrier travels as NATS headers.** The relay sets it on the published message beside
    `Nats-Msg-Id`. JetStream stores headers with the message, so every redelivery carries the same
    context: each delivery attempt is its own processing span, linked to the same producer, with
    `delivery_attempt` as an attribute.
83. **Telemetry never touches idempotency.** The NATS message ID (`<TypeID>.<event>`, the stream's
    deduplication key) and the outbox's `msg_id` are the system's correctness mechanism; trace
    context is observation only. No trace or span ID in a message ID, no message ID derived from
    trace context, and nothing in processing reads trace context to decide anything. A message
    republished after a relay crash carries the same message ID and is dropped by the stream however
    its trace headers differ; a missing or malformed `traceparent` changes nothing but the trace.
84. **Trace context is neither accepted from nor sent to outside parties.** The client API's
    callers and the provider (its webhooks, and the submitter's calls to it) are outside the system:
    an incoming `traceparent` would let a caller choose our trace IDs and, with sampling, our
    sampling decisions, and an outgoing one exposes our internal trace IDs to a third party. So the
    API starts a new trace for every request (the incoming context, if any, is ignored), and the
    submitter's provider calls carry no trace headers. Inside the system, through the outbox and the
    stream, context propagates as decisions 80–82 say. Considered: recording an incoming context as a
    span link (harmless, but nothing here would use it). Correlation across the boundary uses identifiers,
    not trace context: the business IDs both sides know (the network transaction ID and the
    provider's event ID for deposits; the withdrawal's ID, sent as the reference and the idempotency
    key, and the provider's transfer ID for withdrawals), all span attributes on our side; our
    `x-request-id`, returned on every response, which a client quotes and our logs tie to the trace;
    and the provider's request ID where it returns one, recorded on the submitter's span and log line,
    so a support request to the provider can quote its own ID (provider-sim sends none, so nothing is
    built for it here).
85. **Don't trace emptiness.** No span for a relay claim that finds nothing or a consumer fetch that
    returns nothing; the database instrumentation's spans for the polling queries are suppressed. An
    idle system would otherwise produce a trace a second per process.
86. **`trace_id` and `span_id` on every log line in a traced context,** so Grafana goes from a log
    line to its trace. Node: a pino `mixin` reading the active span. Go: slog sees a context only
    through its `...Context` calls, which the code mostly doesn't use, so the IDs are bound where the
    request's and the delivery's loggers already are (the access middleware, `Delivery.Bind`).
87. **A telemetry contract,** `contracts/telemetry.md` (see *The telemetry contract*), shared by both
    implementations: names by OpenTelemetry's semantic conventions, bounded cardinality, no personal
    data, telemetry apart from idempotency. Worth having with one implementation too: it's what keeps
    dashboards and alerts working when the code changes.
88. **Alerting never depends on what telemetry can drop.** Metrics aren't sampled (a counter counts
    every event, kept trace or not), but telemetry can be lost under overload and on a crash. So
    alerts for stuck items and suspense will come from the database's state (*Next*: the
    `postgres_exporter` gauges), independent of the processes and their telemetry; event metrics are
    for rates and durations. Metrics used for alerting are emitted directly, never derived from
    sampled traces. The one metric built here, `ledger_transitions_total{entity, from, to,
    implementation}`, counts applied transitions only, after the commit (a duplicate delivery counts
    nothing, a rolled-back transaction nothing): its labels are the closed set of legal transitions,
    so its cardinality is bounded.
89. **No sampling.** Every trace is kept: in a demo, and in production too until volume or cost makes
    sampling necessary (then parent-based plus tail sampling, *Next*).
90. **Resource attributes:** `service.name` the process's role, the same in both implementations (a
    mixed stack shows one service per role); `implementation` (`node` or `go`) as in the logs;
    `service.instance.id`, a random UUID generated once per process start; `host.name`; the SDK's
    own attributes (`telemetry.sdk.language` and the like) as they come; nothing else (no host or
    process detectors). No `service.version`: the images carry no build version.
    - `service.instance.id` tells two instances of one service apart (two deposit-workers of one
      implementation): without it they write identical metric series, each pushing its own
      cumulative counter, so the series flips between their values and `rate()` reads each drop as
      a reset. A random UUID, the semantic conventions' recommendation: always unique, the same logic
      in both implementations, independent of the platform. Considered: the hostname as the instance
      ID (more readable, but unique only if the platform makes it so).
    - `host.name` maps an instance to its container, for a person: in compose the container ID, in
      Kubernetes the pod name. The one host attribute admitted on purpose (a container ID is not
      personal data); still no host or process detectors otherwise.

    Confirmed in step 1 (`contracts/telemetry.md`); the two instance attributes added in step 3, which
    the transition counter and its dashboard depend on.
91. **Equivalence for telemetry is the contract and the same tests,** not bytes: both suites assert the
    same span names, kinds, attributes and links with in-memory exporters (Node
    `InMemorySpanExporter`, Go `tracetest.SpanRecorder`), and a mixed stack shows the links cross
    implementations. Optional, if cheap: a script that queries the local Tempo after the demo and
    checks that each processing span has a link to an existing request trace.
92. **Tag `v0.5-observability`** on the merge commit.
93. **Logs go to stdout and, as a copy, to the telemetry backend.** Stdout's JSON lines stay the
    source of truth: what `docker compose logs`, the scripts and the e2e tests read, and what a
    platform ships wherever it ships logs (CloudWatch, Datadog, …), complete with the backend down.
    Each line is also exported over OTLP, so Grafana shows a request's log lines with its trace
    (Loki beside Tempo); that copy is telemetry like the rest, batched and droppable (decision 79).
    From the processes rather than by shipping the containers' output: otel-lgtm doesn't read
    Docker's logs, so that would mean another collector container, more infrastructure than the
    copy is worth here. The privacy rule applies to both outputs alike (log lines carry IDs, never
    personal data). Per environment: the copy is switched by OpenTelemetry's standard
    `OTEL_LOGS_EXPORTER`: `otlp` (the specification's default, and compose's) for stdout and the
    copy; `none` for stdout only, where an agent ships stdout (a Datadog agent, a Collector reading
    the containers' output). Nothing is lost by `none`: every line carries its `trace_id` and
    `span_id` (decision 86), so the agent's backend links logs and traces the same way. Considered: OTLP only (loses the logs whenever the backend does); shipping
    stdout with Grafana Alloy or a Collector's file receiver (no code change, but another
    container).
