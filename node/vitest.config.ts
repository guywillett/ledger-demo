import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    // Integration tests run against the compose services; these defaults match
    // docker-compose.yml and can be overridden from the environment. The database is
    // ledger_test (migrated by the migrate-test service), never the development database.
    env: {
      DATABASE_URL:
        process.env.DATABASE_URL ??
        'postgres://ledger:ledger@localhost:5432/ledger_test?sslmode=disable',
      NATS_URL: process.env.NATS_URL ?? 'nats://localhost:4222',
      LOG_LEVEL: process.env.LOG_LEVEL ?? 'silent',
    },
  },
});
