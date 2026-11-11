-- migrate:up

-- Slice 5 decision 81: the trace context of the request that enqueued an event, so the relay's
-- publish span can link to it. The string map the propagators inject into (W3C: traceparent,
-- tracestate, baggage), stored as it is: adding a propagator needs no schema change. Observation
-- only, never read to decide anything (decision 83). Null for an event enqueued outside a traced
-- context (telemetry off, tests) and for every row from before this migration: no backfill.
ALTER TABLE outbox ADD COLUMN trace_context jsonb;

-- migrate:down

ALTER TABLE outbox DROP COLUMN trace_context;
