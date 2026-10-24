-- migrate:up

-- POST /provider/incoming-payments follows the webhook contract (slice 2 decisions 37-38): what
-- it used to refuse is now recorded.

-- Suspense: an unmatched payment has arrived at the bank but matches none of our accounts. It is
-- recorded (money can't be refused over HTTP), books nothing and publishes no event. Resolving
-- it (matching it to an account, or returning it) is a manual process, out of scope. The one
-- idempotency key, UNIQUE (network, network_transaction_id), keeps covering every
-- notification, matched or not: a provider retry with the same details is a duplicate of the
-- suspense record, even if the IBAN has gained an account since. Resolution is never automatic.
-- The account check is a biconditional: an unmatched payment has no account, and a payment
-- without an account is unmatched, so resolving one (giving it an account) must be an explicit
-- status change, never a quiet UPDATE of account_id.
ALTER TABLE incoming_payments
    ALTER COLUMN account_id DROP NOT NULL,
    DROP CONSTRAINT incoming_payments_status_check,
    ADD CONSTRAINT incoming_payments_status_check
        CHECK (status IN ('created', 'received', 'under_review', 'approved', 'rejected',
                          'credited', 'returned', 'unmatched')),
    ADD CONSTRAINT incoming_payments_unmatched_check
        CHECK ((account_id IS NULL) = (status = 'unmatched'));

ALTER TABLE payment_status_history
    DROP CONSTRAINT payment_status_history_from_status_check,
    DROP CONSTRAINT payment_status_history_to_status_check,
    ADD CONSTRAINT payment_status_history_from_status_check
        CHECK (from_status IN ('created', 'received', 'under_review', 'approved', 'rejected',
                               'credited', 'returned', 'unmatched')),
    ADD CONSTRAINT payment_status_history_to_status_check
        CHECK (to_status IN ('created', 'received', 'under_review', 'approved', 'rejected',
                             'credited', 'returned', 'unmatched'));

-- The provider sent the same network transaction ID with other details: recorded and alerted,
-- changing nothing. A log (bigserial key), one row per conflicting notification received.
CREATE TABLE payment_notification_conflicts (
    id                   bigserial   PRIMARY KEY,
    -- The payment stored under that network transaction ID.
    payment_id           uuid        NOT NULL REFERENCES incoming_payments (id),
    -- The details received.
    creditor_iban        text        NOT NULL,
    provider_account_id  text,
    amount_minor         bigint      NOT NULL,
    currency             text        NOT NULL,
    -- Which compared fields differ from the stored payment (names, never values).
    differing_fields     text[]      NOT NULL,
    received_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_notification_conflicts_payment_idx
    ON payment_notification_conflicts (payment_id);

-- migrate:down

-- Fails while unmatched payments exist: their history rows are append-only and can't be removed.
-- Reset the database instead (see the README).
DROP TABLE payment_notification_conflicts;

ALTER TABLE payment_status_history
    DROP CONSTRAINT payment_status_history_from_status_check,
    DROP CONSTRAINT payment_status_history_to_status_check,
    ADD CONSTRAINT payment_status_history_from_status_check
        CHECK (from_status IN ('created', 'received', 'under_review', 'approved', 'rejected',
                               'credited', 'returned')),
    ADD CONSTRAINT payment_status_history_to_status_check
        CHECK (to_status IN ('created', 'received', 'under_review', 'approved', 'rejected',
                             'credited', 'returned'));

ALTER TABLE incoming_payments
    DROP CONSTRAINT incoming_payments_unmatched_check,
    DROP CONSTRAINT incoming_payments_status_check,
    ADD CONSTRAINT incoming_payments_status_check
        CHECK (status IN ('created', 'received', 'under_review', 'approved', 'rejected',
                          'credited', 'returned')),
    ALTER COLUMN account_id SET NOT NULL;
