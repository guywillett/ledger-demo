import { defineConfig } from 'vitest/config';
import { testEnv } from './vitest.config.ts';

// Process-level tests (npm run test:e2e): they start the real deposit-worker and relay as child
// processes and kill them, so they take seconds each. Run one file at a time, and not while
// `npm test` runs (both use the test NATS stream, which these tests purge first, and the test
// outbox, which they drain).
//
// Only the environment is shared with vitest.config.ts: its projects would replace `include`.
export default defineConfig({
  test: {
    env: testEnv,
    include: ['test/e2e/**/*.e2e.ts'],
    testTimeout: 60_000,
    hookTimeout: 30_000,
    fileParallelism: false,
  },
});
