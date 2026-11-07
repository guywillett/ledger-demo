import type { Queryable } from '../shared/db.ts';
import type { Iban } from '../shared/iban.ts';
import type { Uuid } from '../shared/ids.ts';

// The account entity and how it is read. Accounts are created only by the seed migration and the
// tests (account creation is out of scope); a new account's zero balance row comes from a trigger
// (ledger/balances.ts reads it).

export type Account = {
  readonly id: Uuid<'acct'>;
  // A unique label (e.g. "alice").
  readonly name: string;
  readonly iban: Iban;
  readonly createdAt: Date;
};

type AccountRow = {
  id: string;
  name: string;
  iban: string;
  created_at: Date;
};

function accountFromRow(row: AccountRow): Account {
  return {
    id: row.id as Uuid<'acct'>,
    name: row.name,
    // The column CHECK guards the format; the seed and the tests insert normalised IBANs.
    iban: row.iban as Iban,
    createdAt: row.created_at,
  };
}

// provider_account_id stays out: a provider identifier, not the account's own (slice 4
// decision 69).
const GET_SQL = 'SELECT id, name, iban, created_at FROM accounts WHERE id = $1';

// undefined if there is no such account.
export async function getAccount(db: Queryable, id: Uuid<'acct'>): Promise<Account | undefined> {
  const result = await db.query<AccountRow>(GET_SQL, [id]);
  const row = result.rows[0];
  return row === undefined ? undefined : accountFromRow(row);
}

const EXISTS_SQL = 'SELECT 1 FROM accounts WHERE id = $1';

// Whether the account exists: for a list's 404 (slice 4 decision 64: a list checks its account,
// so an empty list means an account with nothing in it). A read before a read: nothing is written
// based on it. Go: accounts.Exists.
export async function accountExists(db: Queryable, id: Uuid<'acct'>): Promise<boolean> {
  const result = await db.query(EXISTS_SQL, [id]);
  return result.rowCount === 1;
}
