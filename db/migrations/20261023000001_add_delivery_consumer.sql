-- migrate:up

-- One delivery log for every consumer (slice 2 decision 36): the deposit-worker and the submitter
-- both write to event_deliveries, and this column says whose delivery a row is, so duplicates and
-- stale in_progress rows (the trace of a crash) can be attributed. Existing rows are the
-- deposit-worker's: it was the only consumer.
--
-- The default only backfills those existing rows, and is dropped at once: every insert must
-- name its consumer, so one that forgets (a new consumer, the Go implementation) fails the NOT
-- NULL instead of being silently counted as the deposit-worker's.
ALTER TABLE event_deliveries
    ADD COLUMN consumer text NOT NULL DEFAULT 'deposit-worker'
        CHECK (consumer IN ('deposit-worker', 'submitter'));
ALTER TABLE event_deliveries
    ALTER COLUMN consumer DROP DEFAULT;

-- migrate:down

ALTER TABLE event_deliveries
    DROP COLUMN consumer;
