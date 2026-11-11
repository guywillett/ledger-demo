# Telemetry contract

What both implementations emit over OpenTelemetry, and the rules every addition follows (slice 5
decision 87). Shared as `openapi.yaml` and `events/` are: the same names, kinds, attributes and
units in Node and Go, asserted by the same tests in both suites with in-memory exporters (Node
`test/telemetry/`, Go `internal/*/telemetry_test.go`). Bytes don't apply: IDs and timings differ by
nature.

Version 4 (slice 5, step 4): the resource, propagation at the edges and through the outbox and
NATS, HTTP server, database and messaging spans, the request duration and the transition counter,
the log fields and the log lines' copy as OTLP log records.

## The rules

- **Names by OpenTelemetry's semantic conventions:** the stable HTTP and database conventions
  (Go's otelhttp and our tracer use semconv v1.43.0; Node's instrumentations the same keys).
  Anything of our own is listed here, with its reason.
- **Bounded cardinality:** no unbounded value (an account, deposit, withdrawal or event ID; an
  amount; a raw path) in a span name or a metric attribute. Span names are route templates and
  statement operations; IDs go in span attributes and log lines. The rule is about values that grow
  within a process, not instance identity: a new `service.instance.id` per restart is a new set of
  series per process lifetime, which is bounded and expected (Prometheus ages stale series out).
- **No personal data:** no IBANs, names or remittance text anywhere, as sent or normalised; IDs
  only. Database spans carry the statement's text, never its parameters, and a failure's code,
  never its message. No request or response bodies, no headers beyond the conventions' safe ones,
  no query string (caller input that could carry anything, so the access log leaves it out too), no
  client IP address or user agent. Extends "events carry no personal data".
- **Telemetry never touches idempotency** (decision 83): no trace or span ID in a message ID, none
  derived from one, and nothing reads trace context to decide anything.
- **No trace context across the system's edges** (decision 84): see *Propagation*.
- **Don't trace emptiness** (decision 85): nothing for an idle relay or consumer.
- **Telemetry can't hurt the work** (decision 79): off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set;
  set, exported in the background in batches, dropped rather than blocking when the backend is slow
  or absent. Export errors are logged at most once a minute (`warn`, "telemetry export failed",
  with `suppressed`: how many were left out since the last line). Shutdown flushes within 2 s. The
  one way found in which it can: an endpoint whose name doesn't resolve, in Node, whose failing DNS
  lookups hold the thread pool its database connections need too (the slice 5 spec's *Known gaps*,
  "Telemetry's DNS can starve Node's thread pool"; mitigated, not cured, by a larger pool,
  `UV_THREADPOOL_SIZE` in the Node image).

## Resource

| Attribute | Value |
|---|---|
| `service.name` | the process's role: `api`, `relay`, `deposit-worker` or `submitter`, the same in both implementations (decision 90), so a mixed stack shows one service per role |
| `implementation` | `node` or `go`, as in the logs (our own attribute: the logs' field) |
| `service.instance.id` | a random UUID, generated once per process start: tells two instances of one service apart (two deposit-workers of one implementation), which would otherwise write identical metric series, each pushing its own cumulative values, so a series flips between them and `rate()` reads each drop as a reset. Random, as the conventions recommend: always unique, the same in both implementations, whatever the platform |
| `host.name` | the process's hostname, so a person can map an instance to its container (in compose the container ID, in Kubernetes the pod name). The one host attribute admitted on purpose: a container ID, not personal data |
| `telemetry.sdk.language`, `telemetry.sdk.name`, `telemetry.sdk.version` | the SDK's own, as they come (`nodejs` or `go`) |

Nothing else: no host or process detectors (`host.name` is set by our code, not detected). `service.version` isn't set: the images carry no build
version yet. Go's SDK also merges `OTEL_RESOURCE_ATTRIBUTES` (its `WithResource` always adds the
environment's), Node's doesn't read it; compose sets none.

Provider-sim and reconciliation have no telemetry (test infrastructure and an auditor).

## Propagation

W3C Trace Context (`traceparent`, `tracestate`) and W3C Baggage (`baggage`), OpenTelemetry's
defaults (decision 80).

- **Incoming requests to the API** start a new trace, whatever trace headers they carry: the API's
  callers (clients, the provider's webhooks) are outside the system (decision 84). Node: the API's
  propagator injects but never extracts; Go: otelhttp with no propagators.
- **The submitter's calls to the provider** carry no trace headers (decision 84): neither
  implementation instruments outgoing HTTP.
- **Inside the system, through the outbox and the stream** (decisions 81 and 82): when the API
  enqueues an event, the current context (the request's span) is injected into a string map and
  stored in the outbox row's `trace_context` (`jsonb`; null outside a traced context, and for rows
  from before slice 5). The relay restores it for its publish span's link, and sends the publish
  span's own context as the message's headers, beside `Nats-Msg-Id`; JetStream stores headers with
  the message, so every redelivery carries the same context. The consumer extracts it for its
  processing span's link. Never in the payload: payloads stay byte-identical across
  implementations.
- **The chain:** each step starts a trace of its own and links to the one that caused it (the
  messaging conventions), so a request's trace ends with the request: process -> publish ->
  request. Grafana follows the links both ways.
- **A missing or malformed carrier changes nothing but the trace:** no link, no error, and the
  event is published and processed as ever. Trace context never touches idempotency (decision 83):
  `Nats-Msg-Id` is the outbox's `msg_id` whatever the headers, so a republished event is dropped
  by the stream however its trace headers differ.

## Spans

### HTTP server (the API)

One per request, the root of its own trace. Kind `SERVER`.

- **Name:** `{method} {route}` for a matched route, e.g. `GET /accounts/{account_id}/deposits`
  (the route template in the contract's `{param}` form, never the raw path); `{method}` alone for a
  request no route matches, and for an unclean path the API answers 404 before routing
  (`/accounts//balance`). `HEAD` on a GET route is `HEAD /…`.
- **Attributes** (both implementations, by the HTTP conventions): `http.request.method`,
  `http.route` (matched routes only), `http.response.status_code`, `url.path`, `url.scheme`,
  `server.address`, `network.peer.port`, `network.protocol.version`. Each SDK adds one more by the
  same conventions: Node `server.port`, Go `http.response.body.size`.
- **Not**, though the conventions record them (the privacy rule): `client.address` and
  `network.peer.address`, IP addresses, personal data under the GDPR; `user_agent.original`, nearly
  so; `url.query`, caller input that could carry anything. Nor headers or bodies. Node's HTTP
  instrumentation never sets them (its start hook); otelhttp can't leave them out, so Go drops them
  at export (`telemetry.WithoutPersonalData`).
- **Status:** an error for a 5xx (the convention for server spans).

The routes: `GET /health`, `GET /accounts/{account_id}`, `GET /accounts/{account_id}/balance`,
`GET /accounts/{account_id}/deposits`, `GET /accounts/{account_id}/deposits/{deposit_id}`,
`POST /accounts/{account_id}/withdrawals`, `GET /accounts/{account_id}/withdrawals`,
`GET /accounts/{account_id}/withdrawals/{withdrawal_id}`,
`GET /accounts/{account_id}/transactions`, `POST /provider/incoming-payments`,
`POST /provider/transfer-updates` (the operations of `openapi.yaml`).

### Database

One per statement run inside a span, its child; none for a statement outside any span (the relay's
and the consumers' polling: decision 85). Kind `CLIENT`. So: the API's statements in their
request's span, the relay's marks in its batch's span, and each delivery's statements in its
processing span; a claim's and a fetch's never.

- **Name:** the statement's operation, its first keyword upper-cased (`SELECT`, `INSERT`, `UPDATE`,
  `WITH`, `BEGIN`, `COMMIT`, …; `QUERY` if it starts with anything else): bounded, as every
  statement is a constant in the code. Our own choice where the conventions allow several (Node's
  pg instrumentation would otherwise name it `pg.query:SELECT ledger`).
- **Attributes:** `db.system.name` (`postgresql`), `db.namespace` (the database), `db.operation.name`
  (the operation, as the name), `db.query.text` (the statement with its `$1` placeholders, never the
  values), `server.address`, `server.port`.
- **On failure:** `error.type` the SQLSTATE code (e.g. `22P02`), and an error status with the
  fixed description `database error`; no exception event, and never Postgres's message, which can
  quote the values sent (22P02's "invalid input syntax for type uuid: …"). Node's pg instrumentation
  records the message, so Node replaces it as the span ends (and takes the SQLSTATE from the
  instrumentation's own sanitised exception event); Go's tracer records only the code.
- **Not:** parameters, connection or pool spans.

### Messaging

By OpenTelemetry's messaging conventions, NATS JetStream as `messaging.system`. Each event span
also has our own `event_id`, and `deposit_id` or `withdrawal_id` (TypeIDs, as in the logs).

**The relay's batch:** `relay batch`, kind `INTERNAL`, the root of a trace of its own, one per claim
that returns events (none for a claim that finds nothing: decision 85). Attributes:
`messaging.system` (`nats`), `messaging.batch.message_count` (how many it claimed). Its children:
each event's publish span, and the marks' database spans.

**Publish:** `publish {subject}` (e.g. `publish deposit.created`), kind `PRODUCER`, a child of the
batch, one per event. Linked to the context stored with the event (the request that enqueued it);
no link when there is none or it is malformed. Attributes: `messaging.system` (`nats`),
`messaging.operation.name` (`publish`), `messaging.operation.type` (`send`),
`messaging.destination.name` (the subject), `messaging.message.id` (the `Nats-Msg-Id`),
`event_id`, `deposit_id` or `withdrawal_id`. A failed publish: status error, `publish failed`. Its
context is the message's trace headers.

**Process:** `process {subject}` (e.g. `process deposit.created`), kind `CONSUMER`, the root of a
trace of its own, one per delivery: a redelivery is another, linked to the same publish, with its
own `delivery_attempt` (decision 82). Linked to the context in the message's headers; no link
when there is none or it is malformed. It starts before anything the delivery does and ends once
the message is settled and its outcome recorded, so the delivery's database spans are its
children and its log lines carry its IDs. Attributes: `messaging.system` (`nats`),
`messaging.operation.name` (`process`), `messaging.operation.type` (`process`),
`messaging.destination.name` (the subject), `messaging.consumer.group.name` (the durable consumer:
`deposit-worker` or `submitter`), `messaging.message.id` (the `Nats-Msg-Id`, when there is one),
our own `delivery_attempt` (1, 2, …), `event_id`, `deposit_id` or `withdrawal_id` (when the body
has them: read before validation, as the delivery log reads `event_id`), and
`delivery_outcome` (`applied`, `duplicate`, `invalid` or `failed`, the delivery log's outcome). An
`invalid` or `failed` delivery: status error, `delivery invalid` or `delivery failed`.

Not traced: a fetch (messages are processed one by one, and an empty fetch is emptiness), the NATS
client's own work.

## Metrics

| Name | Type | Unit | Attributes | Recorded |
|---|---|---|---|---|
| `http.server.request.duration` | histogram | `s` | `http.request.method`, `http.route` (matched routes), `http.response.status_code` | for every API request, by the HTTP instrumentation |
| `ledger_transitions_total` | counter | `{transition}` | `entity` (`deposit`, `withdrawal`), `from`, `to` (the statuses; `from` is `none` for a creation), `implementation` (`node`, `go`) | once per applied status transition, and per creation, after its transaction commits |

**`ledger_transitions_total`** (decision 88): the pipeline's flow, one count per step an entity
takes, by the step. A creation is a step from `none` to the entity's first status (`created` or
`unmatched` for a deposit, `held` for a withdrawal), so the pipeline's entry is counted too.
Counted only once its transaction has committed (an after-commit hook), so a rolled-back
transaction counts nothing, and never for a transition that didn't apply (already done: a
duplicate delivery) or a notification that created nothing (a duplicate, a conflict). Bounded:
its attribute values are the legal transitions and the two implementations. A metric for rates;
alerts on stuck items come from the database (decision 88). `implementation` is an attribute of
its own, besides the resource's: Prometheus (otel-lgtm's) puts resource attributes on
`target_info` only, not on each series, so it saves every query by implementation a join on
`target_info` (by `job` and `instance`, the latter from `service.instance.id`).
Nothing else is exported: each implementation keeps an allow-list (metric views), so an
instrumentation's other instruments (pg's connection and operation metrics, otelhttp's body sizes)
are dropped, and a listed metric keeps only its listed attributes. Exported every
`OTEL_METRIC_EXPORT_INTERVAL` ms (compose: 10 s; the SDKs' default 60 s).

## Logs

Every log line written in a traced context carries `trace_id` (32 hex digits) and `span_id` (16),
so a line leads to its trace (decision 86): Node through the logger's mixin (the active span), Go
where a request's and a delivery's loggers are made. None outside a traced context, or with
telemetry off.

Stdout's JSON lines are the source of truth (decision 93): what `docker compose logs`, the scripts
and the e2e tests read, and what a platform's agent ships; unchanged, byte for byte, by what
follows. Each line is also exported, as a copy, as an OTLP log record: telemetry like the rest,
batched and dropped rather than blocking (the rules above), so the backend may miss lines that
stdout has. `OTEL_LOGS_EXPORTER` switches the copy per environment: `otlp` (the specification's
default, and compose's) or `none` (stdout only, where an agent ships stdout: nothing is lost, as
every line carries its `trace_id` and `span_id`). No other value is accepted; and no copy while
telemetry is off. The SDK's own error lines ("telemetry export failed") aren't copied in Node,
whose SDK has a logger of its own (they are mostly the exports failing); Go's are, its SDK logging
through the process's logger. Harmless either way: at most one a minute.

The record, from the line:

| Record field | From the line |
|---|---|
| timestamp | `time` |
| severity number | `level`: `trace` 1 (TRACE), `debug` 5 (DEBUG), `info` 9 (INFO), `warn` 13 (WARN), `error` 17 (ERROR), `fatal` 21 (FATAL) |
| severity text | `level`, as on stdout (`info`) |
| body | `msg`, a string |
| trace ID, span ID | `trace_id` and `span_id`, the record's own fields; not repeated as attributes. Trace flags: Node's from the span; none on Go's when the IDs come from the logger's bound fields (they carry no flags) |
| attributes | every other field, by the name it has on stdout (`deposit_id`, `request_id`, `attr.level` …), its value as written (an amount a digit string); except `service` and `implementation`, which are the resource's, and `err` |
| `exception.type`, `exception.message` | `err`: its type (each language's own: Node the class name, `OverdraftError`; Go the package path and type name) and message. Node adds `exception.stacktrace` (Go errors carry no stack) |

The scope is each implementation's logger (`ledger-demo/logger`; Go's
`github.com/guywillett/ledger-demo/go/internal/logging`). The privacy rule applies to the copy as
to stdout: log lines carry IDs, never personal data.

How each gets there: Node writes every line to a pino multistream, stdout and a copy that turns the
line into the record (`src/shared/logger.ts`). Go's handler is slog's multi-handler of the JSON
handler and OpenTelemetry's `otelslog` bridge (`internal/logging`); a log processor
(`internal/telemetry`) then puts the bound `trace_id` and `span_id` into the record's own fields
(most Go code logs without a context, which is where `otelslog` looks) and the level's name into the
severity text (`otelslog` writes slog's, `INFO`, and `DEBUG-4` for trace).
