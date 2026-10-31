# One image for every Node process; compose picks the command. TypeScript runs directly (type
# stripping), so there is no build stage, only dependencies.

FROM node:26.12.0-alpine3.24 AS deps
WORKDIR /app
COPY package.json package-lock.json .npmrc ./
# Production dependencies only, exactly as locked. No install scripts: none are needed, and
# not running them is safer.
RUN npm ci --omit=dev --ignore-scripts

FROM node:26.12.0-alpine3.24
ENV NODE_ENV=production
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY package.json ./
COPY src ./src
# Operational scripts (e.g. reconciliation: the reconcile compose service runs scripts/reconcile.ts).
COPY scripts ./scripts
# The processes' commands (api, relay, deposit-worker, submitter), the Go image's binary names, so
# compose runs either implementation with the same command (slice 3 decision 51). Each is a one-line
# JavaScript file with a #!/usr/bin/env node shebang that imports its entry point: no shell, so node
# is the process compose starts, not a child of sh (which would not pass SIGTERM on, and graceful
# shutdown would never run). Extensionless, they load as ES modules only because /app/package.json
# has "type": "module" (Node decides by the nearest package.json): without it the import is a
# syntax error, so that setting mustn't go. The mode is set here, not taken from the repository:
# COPY keeps the checkout's mode and the copies are root's, so a strict umask (077) would give
# 0700 files that USER node can't run.
COPY --chmod=0755 bin ./bin
ENV PATH=/app/bin:$PATH
# The image's unprivileged user rather than root.
USER node
# Node handles SIGTERM itself (see src/api/main.ts), so docker compose stop shuts it down cleanly.
# Compose runs each service with init: true, so a minimal init is PID 1 and node isn't (see
# docker-compose.yml).
CMD ["api"]
