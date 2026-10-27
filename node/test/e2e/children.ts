import { spawn, type ChildProcess } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import { jetstreamManager } from '@nats-io/jetstream';
import type pg from 'pg';
import { STREAMS, type NatsConnection } from '../../src/shared/nats.ts';
import { drainOutbox, testStreamPublisher } from '../helpers.ts';

// Helpers for the process-level tests: real services as child processes, and a clean start.

export type Child = {
  readonly process: ChildProcess;
  // Its JSON log lines so far.
  readonly lines: () => Record<string, unknown>[];
  readonly exited: Promise<number | null>;
};

// Starts `script` (e.g. src/deposit-worker/main.ts) and resolves once it has logged `ready`.
export function startChild(
  script: string,
  ready: string,
  env: Record<string, string>,
): Promise<Child> {
  const child = spawn(process.execPath, [script], {
    // process.env already holds the test DATABASE_URL and NATS_URL (vitest config).
    env: { ...process.env, LOG_LEVEL: 'info', ...env },
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  let output = '';
  child.stdout.on('data', (chunk: Buffer) => {
    output += chunk.toString();
  });
  const lines = (): Record<string, unknown>[] =>
    output
      .split('\n')
      .filter((line) => line.startsWith('{'))
      .map((line) => JSON.parse(line) as Record<string, unknown>);
  const exited = new Promise<number | null>((resolve) => child.once('exit', resolve));
  const started = { process: child, lines, exited };
  return waitFor(() => lines().some((l) => l.msg === ready), `${script} start`).then(() => started);
}

// SIGKILL: no chance to ack, nak or record an outcome. A no-op if it has already exited.
export async function kill(child: Child): Promise<void> {
  if (child.process.exitCode === null && child.process.signalCode === null) {
    child.process.kill('SIGKILL');
    await child.exited;
  }
}

export async function waitFor(
  condition: () => boolean | Promise<boolean>,
  what: string,
  timeoutMs = 30_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!(await condition())) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(100);
  }
}

// The name nats-test runs with (docker-compose.yml). Checked before purging, so a test NATS
// URL pointing at the development server by mistake can't empty its streams.
const TEST_NATS_SERVER_NAME = 'ledger-demo-test';

// Starts from an empty outbox backlog and empty test streams: the integration tests leave
// unpublished events (every deposit and withdrawal they create has one) and published ones,
// which would otherwise be the consumers' first backlog. The backlog is published, then purged
// with the rest. Only the test NATS is touched.
export async function cleanStart(pool: pg.Pool, nc: NatsConnection): Promise<void> {
  const serverName = nc.info?.server_name;
  if (serverName !== TEST_NATS_SERVER_NAME) {
    throw new Error(
      `Refusing to purge streams on NATS server "${String(serverName)}": ` +
        `the e2e tests only run against ${TEST_NATS_SERVER_NAME} (nats-test).`,
    );
  }
  await drainOutbox(pool, testStreamPublisher(nc));
  const jsm = await jetstreamManager(nc);
  for (const stream of Object.values(STREAMS)) await jsm.streams.purge(stream);
}
