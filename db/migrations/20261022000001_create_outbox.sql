-- migrate:up

-- Transactional outbox (slice 2, Part A). Every event is inserted here in the same transaction
-- as the state change it announces; the relay (node/src/relay/) publishes it to JetStream and
-- marks it published. Rows are kept after publishing (slice 2 decision 28): a record of exactly
-- what was published and when.
CREATE TABLE outbox (
    -- The event's event_id, so an event keeps one ID however often it is published. No default:
    -- as for event_deliveries.event_id, event IDs always come from the application.
    id             uuid        PRIMARY KEY
        CHECK (uuid_extract_version(id) = 7),
    -- What the event is about, mirroring balance_movements.reference_type/id: reconciliation
    -- joins outbox rows to payments here, without parsing TypeIDs out of the payload.
    entity_type    text        NOT NULL
        CHECK (entity_type IN ('payment')),
    entity_id      uuid        NOT NULL
        CHECK (uuid_extract_version(entity_id) = 7),
    subject        text        NOT NULL,
    -- The Nats-Msg-Id, <entity TypeID>.<status>. An entity enters a status once, so one event
    -- per entity and status: a backstop, since the conditional transitions already ensure it.
    msg_id         text        NOT NULL UNIQUE,
    -- The event as built by the application. jsonb keeps the content, not the text: key order
    -- and whitespace are not preserved, and the relay re-serialises the payload when it
    -- publishes. The published JSON therefore has the same content, but its keys may be in
    -- another order. Consumers and the contract tests don't depend on key order.
    payload        jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- The relay's lease: a claimed row is not claimed again before this time. NULL: never
    -- claimed, or published.
    claimed_until  timestamptz,
    -- Publish attempts (incremented on each claim), for alerting on a stuck event.
    attempts       int         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error     text,
    -- Set once the stream has acknowledged the event.
    published_at   timestamptz
);

-- The relay's claim query reads only unpublished rows, in id order. A partial index keeps that
-- fast however many published rows accumulate.
CREATE INDEX outbox_unpublished_idx
    ON outbox (id) WHERE published_at IS NULL;

CREATE INDEX outbox_entity_idx
    ON outbox (entity_type, entity_id);

-- migrate:down

DROP TABLE outbox;
