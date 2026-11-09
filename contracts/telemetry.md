# Telemetry contract

What both implementations emit over OpenTelemetry, and the rules every addition follows (slice 5
decision 87). Shared as `openapi.yaml` and `events/` are: the same names, kinds, attributes and
units in Node and Go, asserted by the same tests in both suites with in-memory exporters (Node
`test/telemetry/`, Go `internal/*/telemetry_test.go`). Bytes don't apply: IDs and timings differ by
nature.

Version 1 (slice 5, step 1): the resource, propagation at the edges, HTTP server and database spans,
one metric, the log fields. Step 2 adds the messaging spans and links; step 3 the transition counter.

## The rules

- **Names by OpenTelemetry's semantic conventions:** the stable HTTP and database conventions
  (Go's otelhttp and our tracer use semconv v1.43.0; Node's instrumentations the same keys).
  Anything of our own is listed here, with its reason.
- **Bounded cardinality:** no unbounded value (an account, deposit, withdrawal or event ID; an
  amount; a raw path) in a span name or a metric attribute. Span names are route templates and
  statement operations; IDs go in span attributes and log lines.
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
| `telemetry.sdk.language`, `telemetry.sdk.name`, `telemetry.sdk.version` | the SDK's own, as they come (`nodejs` or `go`) |

Nothing else: no host or process detectors. `service.version` isn't set: the images carry no build
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
- Inside the system, through the outbox and the stream: step 2.

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
and the consumers' polling: decision 85). Kind `CLIENT`. In step 1 that means the API's statements
only; the workers' come with their processing spans (step 2).

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

## Metrics

| Name | Type | Unit | Attributes | Recorded |
|---|---|---|---|---|
| `http.server.request.duration` | histogram | `s` | `http.request.method`, `http.route` (matched routes), `http.response.status_code` | for every API request, by the HTTP instrumentation |

Nothing else is exported: each implementation keeps an allow-list (metric views), so an
instrumentation's other instruments (pg's connection and operation metrics, otelhttp's body sizes)
are dropped, and a listed metric keeps only its listed attributes. Exported every
`OTEL_METRIC_EXPORT_INTERVAL` ms (compose: 10 s; the SDKs' default 60 s).

## Logs

Every log line written in a traced context carries `trace_id` (32 hex digits) and `span_id` (16),
so a line leads to its trace (decision 86): Node through the logger's mixin (the active span), Go
where a request's and a delivery's loggers are made. None outside a traced context, or with
telemetry off. Logs still go to stdout as JSON, not over OTLP.
