import { z } from 'zod';
import { loadSharedConfig, parseEnv, type SharedConfig } from '../shared/config.ts';

const apiEnvSchema = z.object({
  // z.coerce: environment variables are always strings.
  PORT: z.coerce.number().int().min(1).max(65535).default(3000),
});

export type ApiConfig = SharedConfig & { readonly port: number };

export function loadApiConfig(env: NodeJS.ProcessEnv = process.env): ApiConfig {
  const shared = loadSharedConfig(env);
  const parsed = parseEnv(apiEnvSchema, env);
  return Object.freeze({ ...shared, port: parsed.PORT });
}
