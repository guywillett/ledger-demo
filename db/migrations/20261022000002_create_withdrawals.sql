-- migrate:up

-- Withdrawals (slice 2, Part B): client-initiated transfers out of an account. Funds are held
-- (available -> held) when the withdrawal is created, then leave (held -> external) when the
-- provider executes it, or go back (held -> available) when it fails.

-- Funds reserved for withdrawals in flight. Named account_balances_held_check by Postgres, which
-- move() maps to an OverdraftError on `held`.
ALTER TABLE account_balances
    ADD COLUMN held bigint NOT NULL DEFAULT 0 CHECK (held >= 0);

CREATE TABLE withdrawals (
    id                    uuid        PRIMARY KEY DEFAULT uuidv7()
        CHECK (uuid_extract_version(id) = 7),
    account_id            uuid        NOT NULL REFERENCES accounts (id),
    -- The client's Idempotency-Key. Unique per account: only a withdrawal that commits takes it
    -- (slice 2 decision 24).
    idempotency_key       text        NOT NULL
        CHECK (char_length(idempotency_key) BETWEEN 1 AND 255),
    amount_minor          bigint      NOT NULL CHECK (amount_minor > 0),
    currency              text        NOT NULL CHECK (currency = 'EUR'),
    -- The destination, normalised as on the other tables; the mod-97 checksum is checked in the
    -- application.
    creditor_iban         text        NOT NULL
        CHECK (creditor_iban ~ '^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$'),
    -- SEPA limits (SEPA Credit Transfer rulebook): the creditor name is required, at most 70
    -- characters; unstructured remittance information at most 140. No remittance information is
    -- NULL, never '': one spelling of "none", so a repeated request compares equal.
    creditor_name         text        NOT NULL
        CHECK (char_length(creditor_name) BETWEEN 1 AND 70),
    remittance_info       text
        CHECK (char_length(remittance_info) BETWEEN 1 AND 140),
    status                text        NOT NULL
        CHECK (status IN ('held', 'submitted', 'executed', 'failed')),
    -- Set exactly when the withdrawal failed.
    failure_reason        text,
    -- The provider's ID for the transfer, once it has accepted it.
    provider_transfer_id  text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'failed') = (failure_reason IS NOT NULL)),
    -- Outgoing text uses the EPC's SEPA basic Latin character set (a-z A-Z 0-9 / - ? : ( ) . , ' +
    -- space), the one every SEPA bank must support. Extended sets (accents, &) exist only by
    -- national agreement, so they are not assumed. Text outside the set is rejected, never
    -- transliterated: what the client sees is exactly what is sent (slice 2 decision 32). The
    -- quote is doubled inside the SQL string; '-' last in the brackets is a literal hyphen. A
    -- name must also contain something other than spaces (\S).
    CONSTRAINT withdrawals_creditor_name_charset_check
        CHECK (creditor_name ~ '^[A-Za-z0-9/?:().,''+ -]+$' AND creditor_name ~ '\S'),
    CONSTRAINT withdrawals_remittance_info_charset_check
        CHECK (remittance_info ~ '^[A-Za-z0-9/?:().,''+ -]+$'),
    UNIQUE (account_id, idempotency_key)
);

-- For listing an account's withdrawals newest first (seek pagination on the ID, as deposits).
CREATE INDEX withdrawals_account_id_idx
    ON withdrawals (account_id, id DESC);

CREATE TABLE withdrawal_status_history (
    id             bigserial   PRIMARY KEY,
    withdrawal_id  uuid        NOT NULL REFERENCES withdrawals (id),
    from_status    text
        CHECK (from_status IN ('held', 'submitted', 'executed', 'failed')),
    to_status      text        NOT NULL
        CHECK (to_status IN ('held', 'submitted', 'executed', 'failed')),
    reason         text,
    at             timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX withdrawal_status_history_withdrawal_idx
    ON withdrawal_status_history (withdrawal_id);

-- Append-only, like deposit_status_history (reject_audit_log_change() is in the schema
-- migration).
CREATE TRIGGER withdrawal_status_history_append_only
    BEFORE UPDATE OR DELETE ON withdrawal_status_history
    FOR EACH ROW EXECUTE FUNCTION reject_audit_log_change();
CREATE TRIGGER withdrawal_status_history_no_truncate
    BEFORE TRUNCATE ON withdrawal_status_history
    FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_log_change();

-- Lock note: ADD CONSTRAINT ... CHECK validates every existing row while holding an ACCESS
-- EXCLUSIVE lock on balance_movements, blocking all ledger writes (and reads) until it is done.
-- Fine at demo size. On a large table the zero-downtime form is ADD CONSTRAINT ... NOT VALID
-- (instant: only new rows are checked) followed by VALIDATE CONSTRAINT, which scans the
-- existing rows under a SHARE UPDATE EXCLUSIVE lock that lets writes carry on.
--
-- balance_movements: the held bucket, and withdrawal references. The reference type and step
-- are checked as a pair (one CHECK instead of one per column), so a deposit can't have a hold
-- step or a withdrawal a credit step. move()'s Movement type mirrors this pairing.
ALTER TABLE balance_movements
    DROP CONSTRAINT balance_movements_from_bucket_check,
    DROP CONSTRAINT balance_movements_to_bucket_check,
    DROP CONSTRAINT balance_movements_reference_type_check,
    DROP CONSTRAINT balance_movements_reference_step_check,
    ADD CONSTRAINT balance_movements_from_bucket_check
        CHECK (from_bucket IN ('external', 'pending_in', 'available', 'held')),
    ADD CONSTRAINT balance_movements_to_bucket_check
        CHECK (to_bucket IN ('external', 'pending_in', 'available', 'held')),
    ADD CONSTRAINT balance_movements_reference_check
        CHECK ((reference_type = 'deposit' AND reference_step IN ('book', 'credit', 'return'))
            OR (reference_type = 'withdrawal' AND reference_step IN ('hold', 'execute', 'release')));

-- withdrawal.held goes through the outbox (enqueued from step 3 on).
ALTER TABLE outbox
    DROP CONSTRAINT outbox_entity_type_check,
    ADD CONSTRAINT outbox_entity_type_check
        CHECK (entity_type IN ('deposit', 'withdrawal'));

-- migrate:down

-- Restoring the old CHECKs fails if withdrawal rows remain in outbox or balance_movements, and
-- balance_movements is append-only (its rows can't be deleted). On such a database, reset it
-- instead (see docs/development.md, Reset).
ALTER TABLE outbox
    DROP CONSTRAINT outbox_entity_type_check,
    ADD CONSTRAINT outbox_entity_type_check
        CHECK (entity_type IN ('deposit'));

ALTER TABLE balance_movements
    DROP CONSTRAINT balance_movements_from_bucket_check,
    DROP CONSTRAINT balance_movements_to_bucket_check,
    DROP CONSTRAINT balance_movements_reference_check,
    ADD CONSTRAINT balance_movements_from_bucket_check
        CHECK (from_bucket IN ('external', 'pending_in', 'available')),
    ADD CONSTRAINT balance_movements_to_bucket_check
        CHECK (to_bucket IN ('external', 'pending_in', 'available')),
    ADD CONSTRAINT balance_movements_reference_type_check
        CHECK (reference_type IN ('deposit')),
    ADD CONSTRAINT balance_movements_reference_step_check
        CHECK (reference_step IN ('book', 'credit', 'return'));

DROP TABLE withdrawal_status_history;
DROP TABLE withdrawals;

ALTER TABLE account_balances
    DROP COLUMN held;
