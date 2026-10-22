-- migrate:up

-- Demo accounts with fixed IDs so the README, tests and curl examples can refer to them.
-- TypeIDs (as used by the API) and IBANs (as used by provider notifications):
--   alice  acct_01m564xr00fx7rjvqryn5dzwxx  FR7612345000010000000000193
--   bob    acct_01m564xr01ecrvnckn84evqsax  FR7612345000010000000000290
--   carol  acct_01m564xr02e70a25p2hkva3gbn  FR7612345000010000000000387
-- The IBANs are made up (bank code 12345, branch 00001) but valid: both the French RIB key
-- and the IBAN mod-97 check digits are correct.
-- alice and bob have provider account IDs; carol has none, as with some vIBAN setups.
-- Each gets its zero balance row from the accounts_create_balance trigger.

INSERT INTO accounts (id, name, iban, provider_account_id) VALUES
    ('01a14c4e-e000-7f4f-896e-f8f54adff3bd', 'alice', 'FR7612345000010000000000193', 'prov_acc_0001'),
    ('01a14c4e-e001-7331-bab2-75411dbbe55d', 'bob',   'FR7612345000010000000000290', 'prov_acc_0002'),
    ('01a14c4e-e002-71c0-a116-c28cf6a1c175', 'carol', 'FR7612345000010000000000387', NULL);

-- migrate:down

-- The balance rows first: they reference the accounts.
DELETE FROM account_balances WHERE account_id IN (
    '01a14c4e-e000-7f4f-896e-f8f54adff3bd',
    '01a14c4e-e001-7331-bab2-75411dbbe55d',
    '01a14c4e-e002-71c0-a116-c28cf6a1c175'
);
DELETE FROM accounts WHERE id IN (
    '01a14c4e-e000-7f4f-896e-f8f54adff3bd',
    '01a14c4e-e001-7331-bab2-75411dbbe55d',
    '01a14c4e-e002-71c0-a116-c28cf6a1c175'
);
