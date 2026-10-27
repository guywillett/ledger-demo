-- migrate:up

-- IDs are UUIDv7. The application generates them (so it knows them before insert);
-- DEFAULT uuidv7() on entity primary keys is only a fallback for manual inserts.
-- CHECK (uuid_extract_version(...) = 7) rejects any non-v7 UUID, whoever supplies it.
--
-- Primary key rule:
--   UUIDv7   for entities that may be referenced or exposed (accounts, deposits, movements).
--   bigserial for internal append-only logs that are never referenced or exposed
--             (deposit_status_history, event_deliveries).
-- updated_at is set explicitly by the application in each UPDATE (no triggers for it).
-- The audit logs (balance_movements, deposit_status_history) are append-only, enforced by
-- triggers at the end of this file.

CREATE TABLE accounts (
    id          uuid        PRIMARY KEY DEFAULT uuidv7()
        CHECK (uuid_extract_version(id) = 7),
    name        text        NOT NULL UNIQUE,
    -- How providers identify the account. Stored normalised (no spaces, upper case);
    -- the format is checked here, the mod-97 checksum in the application.
    iban        text        NOT NULL UNIQUE
        CHECK (iban ~ '^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$'),
    -- The provider's own ID for this account, when it has one (some providers don't, e.g.
    -- for vIBANs). Only unique per provider; there is a single provider in this slice.
    provider_account_id  text  UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE account_balances (
    account_id  uuid        PRIMARY KEY REFERENCES accounts (id),
    pending_in  bigint      NOT NULL DEFAULT 0 CHECK (pending_in >= 0),
    available   bigint      NOT NULL DEFAULT 0 CHECK (available >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Every account gets its zero balance row in the same statement that creates it. move()
-- updates balance rows but never creates them, and GET balance treats a missing row as a
-- missing account, so the row must exist. A trigger rather than application code, so it holds
-- whoever inserts the account (seed migration, tests, psql). It doesn't stop the row being
-- deleted later, or a superuser disabling triggers: reconciliation reports any account without
-- a balance row.
CREATE FUNCTION create_account_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO account_balances (account_id) VALUES (NEW.id);
    RETURN NULL; -- ignored for AFTER triggers
END;
$$;

CREATE TRIGGER accounts_create_balance
    AFTER INSERT ON accounts
    FOR EACH ROW EXECUTE FUNCTION create_account_balance();

CREATE TABLE deposits (
    id                      uuid        PRIMARY KEY DEFAULT uuidv7()
        CHECK (uuid_extract_version(id) = 7),
    -- The IBAN credited, exactly as the provider sent it (normalised): the audit trail of
    -- what we were told. account_id is our mapping of it.
    creditor_iban           text        NOT NULL
        CHECK (creditor_iban ~ '^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$'),
    -- The provider's account ID as sent in the notification, if any. Stored for support and
    -- reconciliation, not used for matching: it may identify a shared master account.
    provider_account_id     text,
    account_id              uuid        NOT NULL REFERENCES accounts (id),
    amount_minor            bigint      NOT NULL CHECK (amount_minor > 0),
    currency                text        NOT NULL CHECK (currency = 'EUR'),
    network                 text        NOT NULL CHECK (network = 'SEPA'),
    network_transaction_id  text        NOT NULL,
    status                  text        NOT NULL
        CHECK (status IN ('created', 'received', 'under_review', 'approved', 'rejected', 'credited', 'returned')),
    notification_count      int         NOT NULL DEFAULT 1 CHECK (notification_count >= 1),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    -- Idempotency key for incoming notifications. A repeat with the same key is a duplicate
    -- only if creditor_iban, amount_minor and currency also match the stored row (compared
    -- directly in the upsert; no request hash, so Node and Go share the exact same rule).
    UNIQUE (network, network_transaction_id)
);

-- GET /accounts/:id/transactions: newest first, paginated by seeking on the ID. UUIDv7 IDs
-- sort by creation time, so the deposit ID is both the order and the cursor
-- (WHERE account_id = $1 AND id < $before ORDER BY id DESC).
CREATE INDEX deposits_account_id_idx
    ON deposits (account_id, id DESC);

CREATE TABLE deposit_status_history (
    id           bigserial   PRIMARY KEY,
    deposit_id   uuid        NOT NULL REFERENCES deposits (id),
    from_status  text
        CHECK (from_status IN ('created', 'received', 'under_review', 'approved', 'rejected', 'credited', 'returned')),
    to_status    text        NOT NULL
        CHECK (to_status IN ('created', 'received', 'under_review', 'approved', 'rejected', 'credited', 'returned')),
    reason       text,
    at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX deposit_status_history_deposit_idx
    ON deposit_status_history (deposit_id);

-- Written only by ledger move(). reference_id is deliberately not a foreign key:
-- it is a generic business reference, so the ledger does not depend on deposits.
-- reference_type says what kind of object reference_id is (and so its TypeID prefix).
-- UUIDv7 PK (not bigserial): this becomes the double-entry transactions table, referenced
-- by entries and exposed as an audit trail, so it is an entity rather than a log.
CREATE TABLE balance_movements (
    id              uuid        PRIMARY KEY DEFAULT uuidv7()
        CHECK (uuid_extract_version(id) = 7),
    account_id      uuid        NOT NULL REFERENCES accounts (id),
    from_bucket     text        NOT NULL
        CHECK (from_bucket IN ('external', 'pending_in', 'available')),
    to_bucket       text        NOT NULL
        CHECK (to_bucket IN ('external', 'pending_in', 'available')),
    amount_minor    bigint      NOT NULL CHECK (amount_minor > 0),
    reference_type  text        NOT NULL
        CHECK (reference_type IN ('deposit')),
    reference_id    uuid        NOT NULL
        CHECK (uuid_extract_version(reference_id) = 7),
    reference_step  text        NOT NULL
        CHECK (reference_step IN ('book', 'credit', 'return')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (from_bucket <> to_bucket),
    -- The ledger's own idempotency guard: each movement is applied once.
    UNIQUE (reference_type, reference_id, reference_step)
);

CREATE INDEX balance_movements_account_idx
    ON balance_movements (account_id);

-- A log, not a guard: no unique constraint on event_id, by design.
CREATE TABLE event_deliveries (
    id                bigserial   PRIMARY KEY,
    -- No default: the event ID always comes from the event itself. NULL only for a message
    -- whose event_id couldn't be read at all: the row is written before validation, so such a
    -- message is in_progress until it is marked invalid (see the CHECK below).
    event_id          uuid
        CHECK (uuid_extract_version(event_id) = 7),
    nats_msg_id       text,
    delivery_attempt  int         NOT NULL CHECK (delivery_attempt >= 1),
    outcome           text        NOT NULL
        CHECK (outcome IN ('in_progress', 'applied', 'duplicate', 'invalid', 'failed')),
    received_at       timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz,
    CHECK (event_id IS NOT NULL OR outcome IN ('in_progress', 'invalid'))
);

CREATE INDEX event_deliveries_event_idx
    ON event_deliveries (event_id);

-- Append-only audit logs: rows are inserted, never changed or removed, whoever connects.
-- event_deliveries is not included: the worker updates each delivery's outcome.
-- (DROP TABLE, as in migrate:down or a database reset, is not affected.)
--
-- Good enough for the demo, where everything connects as one user that owns the tables. In
-- production the primary control would be database roles: migrations run as an owner role,
-- and each service connects as its own role granted only what it needs (e.g. INSERT and
-- SELECT on these tables, no UPDATE, DELETE or TRUNCATE; the API no write access to balances
-- at all). Triggers then remain a backstop against mistakes by privileged users, who can
-- otherwise do anything (a superuser can even disable triggers).
CREATE FUNCTION reject_audit_log_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP;
END;
$$;

CREATE TRIGGER balance_movements_append_only
    BEFORE UPDATE OR DELETE ON balance_movements
    FOR EACH ROW EXECUTE FUNCTION reject_audit_log_change();
CREATE TRIGGER balance_movements_no_truncate
    BEFORE TRUNCATE ON balance_movements
    FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_log_change();

CREATE TRIGGER deposit_status_history_append_only
    BEFORE UPDATE OR DELETE ON deposit_status_history
    FOR EACH ROW EXECUTE FUNCTION reject_audit_log_change();
CREATE TRIGGER deposit_status_history_no_truncate
    BEFORE TRUNCATE ON deposit_status_history
    FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_log_change();

-- migrate:down

DROP TABLE event_deliveries;
DROP TABLE balance_movements;
DROP TABLE deposit_status_history;
DROP TABLE deposits;
DROP TABLE account_balances;
DROP TABLE accounts;
DROP FUNCTION create_account_balance();
DROP FUNCTION reject_audit_log_change();
