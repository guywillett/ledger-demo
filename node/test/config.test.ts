import { describe, expect, it } from 'vitest';
import { loadSharedConfig } from '../src/shared/config.ts';

describe('loadSharedConfig', () => {
  it('parses a valid environment and defaults LOG_LEVEL', () => {
    const config = loadSharedConfig({
      DATABASE_URL: 'postgres://u:p@localhost:5432/db',
      NATS_URL: 'nats://localhost:4222',
    });
    expect(config).toEqual({
      databaseUrl: 'postgres://u:p@localhost:5432/db',
      natsUrl: 'nats://localhost:4222',
      logLevel: 'info',
    });
    expect(Object.isFrozen(config)).toBe(true);
  });

  it('reports every invalid variable at once', () => {
    expect(() => loadSharedConfig({ NATS_URL: 'not a url', LOG_LEVEL: 'loud' })).toThrow(
      /DATABASE_URL[\s\S]*NATS_URL[\s\S]*LOG_LEVEL/,
    );
  });
});
