import { describe, expect, it } from 'vitest';
import { peekEntityIds } from '../../src/consumer/messages.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';

// What the processing span reads from a message before it is validated (slice 5).

describe('peekEntityIds', () => {
  const deposit = toTypeId('dep', newUuid('dep'));
  const withdrawal = toTypeId('wd', newUuid('wd'));

  it('reads a deposit_id and a withdrawal_id that are TypeIDs of their kind', () => {
    expect(
      peekEntityIds(JSON.stringify({ deposit_id: deposit, withdrawal_id: withdrawal })),
    ).toEqual({
      deposit_id: deposit,
      withdrawal_id: withdrawal,
    });
  });

  it.each([
    ['another kind', { deposit_id: withdrawal }],
    ['not a TypeID', { deposit_id: 'dep_nope' }],
    ['not a string', { deposit_id: 42 }],
    ['absent', { event_id: 'x' }],
  ])('leaves out one that is %s, as Go does', (_case, fields) => {
    expect(peekEntityIds(JSON.stringify(fields))).toEqual({});
  });

  it('reads nothing from a body that is not a JSON object', () => {
    expect(peekEntityIds('not json')).toEqual({});
    expect(peekEntityIds('"a string"')).toEqual({});
  });
});
