import { defineConfig } from 'vitest/config';

// Integration tests run against the compose services: the test database (ledger_test,
// migrated by migrate-test) and the test NATS (nats-test), never the development ones.
//
// The tests' own variables are TEST_DATABASE_URL, TEST_NATS_URL and TEST_LOG_LEVEL, mapped to
// the DATABASE_URL / NATS_URL / LOG_LEVEL the code reads. So a DATABASE_URL exported in your
// shell for development work can never redirect a test run at development data.
const databaseUrl =
  process.env.TEST_DATABASE_URL ??
  'postgres://ledger:ledger@localhost:5432/ledger_test?sslmode=disable';

// Guard: tests create accounts and payments, so refuse anything but a *_test database.
const databaseName = new URL(databaseUrl).pathname.slice(1);
if (!databaseName.endsWith('_test')) {
  throw new Error(
    `Refusing to run tests against database "${databaseName}": its name must end in _test.`,
  );
}

export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    env: {
      DATABASE_URL: databaseUrl,
      // nats-test runs with server name ledger-demo-test, which the e2e tests check before
      // purging its stream.
      NATS_URL: process.env.TEST_NATS_URL ?? 'nats://localhost:4223',
      LOG_LEVEL: process.env.TEST_LOG_LEVEL ?? 'silent',
    },
  },
});
