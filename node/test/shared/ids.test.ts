import { describe, expect, it } from 'vitest';
import { fromTypeId, newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';

describe('ids', () => {
  it('generates UUIDv7', () => {
    expect(newUuid('acct')).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );
  });

  it('round-trips a UUID through a TypeID', () => {
    const uuid = newUuid('dep');
    const typeId = toTypeId('dep', uuid);
    expect(typeId).toMatch(/^dep_[0-9a-z]{26}$/);
    expect(fromTypeId('dep', typeId)).toBe(uuid);
  });

  it('rejects a TypeID with the wrong prefix', () => {
    const typeId = toTypeId('acct', newUuid('acct'));
    expect(() => fromTypeId('dep', typeId)).toThrow();
  });

  it('rejects malformed TypeIDs', () => {
    expect(() => fromTypeId('dep', 'dep_not-a-typeid')).toThrow();
    expect(() => fromTypeId('dep', '01m564xr00fx7rjvqryn5dzwxx')).toThrow();
  });

  it('rejects a TypeID that does not hold a UUIDv7', () => {
    const v4 = '2c5ea4c0-4067-41fb-9c1e-5c3f2d7e8a90' as Uuid<'acct'>;
    expect(() => fromTypeId('acct', toTypeId('acct', v4))).toThrow(/UUIDv7/);
  });

  it('keeps entity IDs apart at compile time', () => {
    const accountId = newUuid('acct');
    // @ts-expect-error: an account UUID is not a deposit UUID
    toTypeId('dep', accountId);
  });

  // The seed migration and docs/development.md, Demo accounts, hard-code these pairs.
  it.each([
    ['01a14c4e-e000-7f4f-896e-f8f54adff3bd', 'acct_01m564xr00fx7rjvqryn5dzwxx'],
    ['01a14c4e-e001-7331-bab2-75411dbbe55d', 'acct_01m564xr01ecrvnckn84evqsax'],
    ['01a14c4e-e002-71c0-a116-c28cf6a1c175', 'acct_01m564xr02e70a25p2hkva3gbn'],
  ])('seed account %s is %s', (uuid, typeId) => {
    expect(toTypeId('acct', uuid as Uuid<'acct'>)).toBe(typeId);
    expect(fromTypeId('acct', typeId)).toBe(uuid);
  });
});
