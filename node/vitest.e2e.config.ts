import { defineConfig } from 'vitest/config';
import base from './vitest.config.ts';

// Process-level tests (npm run test:e2e): they start the real worker as a child process and
// kill it, so they take seconds each. Run one file at a time, and not while `npm test` runs
// (both use the test NATS stream, which these tests purge first).
//
// The base test options (env: the test database and test NATS) are spread in rather than
// merged with mergeConfig, which would concatenate the `include` arrays.
export default defineConfig({
  test: {
    ...base.test,
    include: ['test/e2e/**/*.e2e.ts'],
    testTimeout: 60_000,
    hookTimeout: 30_000,
    fileParallelism: false,
  },
});
