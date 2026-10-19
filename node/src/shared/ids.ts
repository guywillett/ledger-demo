import { TypeID, typeid } from 'typeid-js';

// IDs are UUIDv7 inside the database and prefixed TypeIDs everywhere else.
// Conversion happens only at boundaries (data-access layer, request and event schemas).

export type IdPrefix = 'acct' | 'pay' | 'evt';

// A UUID as stored in Postgres, branded with the entity it identifies. The brand stops a
// TypeID (or any other string) being passed where a database UUID is expected, and the
// entity parameter stops one entity's UUID being used as another's (Uuid<'acct'> is not
// a Uuid<'pay'>). Brands exist only at compile time; at runtime this is a plain string.
export type Uuid<P extends IdPrefix> = string & {
  readonly __brand: 'Uuid';
  readonly __entity: P;
};

// A TypeID string such as "pay_01m5…", typed by its prefix.
export type TypeIdString<P extends IdPrefix> = `${P}_${string}`;

// A new UUIDv7, for inserting a new row.
export function newUuid<P extends IdPrefix>(prefix: P): Uuid<P> {
  return typeid(prefix).toUUID() as Uuid<P>;
}

// NoInfer: P is inferred from `prefix` only. Without it, TypeScript would also infer P
// from `uuid` and widen it to fit both, so toTypeId('pay', accountUuid) would compile
// with P = 'pay' | 'acct'.
export function toTypeId<P extends IdPrefix>(prefix: P, uuid: Uuid<NoInfer<P>>): TypeIdString<P> {
  return TypeID.fromUUID(prefix, uuid).toString() as TypeIdString<P>;
}

// Parses a TypeID from outside, checking its prefix and that it holds a UUIDv7.
// Throws on anything else.
export function fromTypeId<P extends IdPrefix>(prefix: P, value: string): Uuid<P> {
  const uuid = TypeID.fromString(value, prefix).toUUID();
  if (uuid[14] !== '7') {
    throw new Error(`TypeID ${value} does not contain a UUIDv7`);
  }
  return uuid as Uuid<P>;
}
