import { TypeID, typeid } from 'typeid-js';

// IDs are UUIDv7 inside the database and prefixed TypeIDs everywhere else.
// Conversion happens only at boundaries (data-access layer, request and event schemas).

export type IdPrefix = 'acct' | 'pay' | 'evt';

// A UUID as stored in Postgres. The brand stops a TypeID (or any other string) being
// passed where a database UUID is expected; it exists only at compile time.
export type Uuid = string & { readonly __brand: 'Uuid' };

// A TypeID string such as "pay_01m5…", typed by its prefix.
export type TypeIdString<P extends IdPrefix> = `${P}_${string}`;

// A new UUIDv7, for inserting a new row.
export function newUuid(): Uuid {
  return typeid().toUUID() as Uuid;
}

export function toTypeId<P extends IdPrefix>(prefix: P, uuid: Uuid): TypeIdString<P> {
  return TypeID.fromUUID(prefix, uuid).toString() as TypeIdString<P>;
}

// Parses a TypeID from outside, checking its prefix and that it holds a UUIDv7.
// Throws on anything else.
export function fromTypeId<P extends IdPrefix>(prefix: P, value: string): Uuid {
  const uuid = TypeID.fromString(value, prefix).toUUID();
  if (uuid[14] !== '7') {
    throw new Error(`TypeID ${value} does not contain a UUIDv7`);
  }
  return uuid as Uuid;
}
