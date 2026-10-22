# One image for the Node processes (the API now, the worker later); compose picks the command.
# TypeScript runs directly (type stripping), so there is no build stage, only dependencies.

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
# Operational scripts (e.g. docker compose run --rm worker node scripts/reconcile.ts).
COPY scripts ./scripts
# The image's unprivileged user rather than root.
USER node
# Node is PID 1 and handles SIGTERM itself (see src/api/main.ts), so docker compose stop
# shuts it down cleanly.
CMD ["node", "src/api/main.ts"]
