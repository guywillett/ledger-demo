-- migrate:up

-- The provider's transfer updates (POST /provider/transfer-updates), one row per distinct
-- provider_event_id. A record, not a guard: the money is decided by the withdrawal's conditional
-- transition (execute / release), in the same transaction. This table only recognises a resent
-- update (notification_count) and keeps contradictions and unmatched updates on file for a
-- person to look at. An internal log, so a bigserial key (slice 1's primary-key rule).
CREATE TABLE transfer_updates (
    id                  bigserial   PRIMARY KEY,
    -- The provider's ID for this update; unique per provider (a single provider so far).
    provider_event_id   text        NOT NULL UNIQUE,
    -- The withdrawal the update's reference names; NULL when it names none (outcome unmatched).
    withdrawal_id       uuid        REFERENCES withdrawals (id),
    -- What the provider sent as the reference (our withdrawal TypeID), kept as text so an
    -- unmatched update keeps it.
    reference           text        NOT NULL,
    transfer_id         text        NOT NULL,
    status              text        NOT NULL CHECK (status IN ('executed', 'failed')),
    reason              text,
    -- When the provider says it happened; received_at is when it reached us.
    occurred_at         timestamptz NOT NULL,
    received_at         timestamptz NOT NULL DEFAULT now(),
    notification_count  int         NOT NULL DEFAULT 1 CHECK (notification_count >= 1),
    -- What the first receipt of this update did. Resends don't change it.
    outcome             text        NOT NULL
        CHECK (outcome IN ('applied', 'duplicate', 'conflict', 'unmatched')),
    CHECK (status = 'failed' OR reason IS NULL),
    CHECK (withdrawal_id IS NOT NULL OR outcome = 'unmatched')
);

CREATE INDEX transfer_updates_withdrawal_idx
    ON transfer_updates (withdrawal_id);

-- migrate:down

DROP TABLE transfer_updates;
