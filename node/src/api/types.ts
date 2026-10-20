import type { IncomingMessage, ServerResponse } from 'node:http';
import type { FastifyInstance, RawServerDefault } from 'fastify';
import type { Logger } from '../shared/logger.ts';

// Our Fastify instance. The logger type is one of FastifyInstance's type parameters, and we
// pass our pino Logger as loggerInstance, so the default FastifyInstance type doesn't match.
export type App = FastifyInstance<RawServerDefault, IncomingMessage, ServerResponse, Logger>;
