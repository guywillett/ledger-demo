#!/usr/bin/env bash
# Waits until a just-started compose stack is ready: each named one-shot setup service (migrate,
# nats-setup, …) has exited successfully, then, if the api service is running, until it answers.
#
#   docker compose up -d --build && scripts/wait-for-stack.sh migrate nats-setup
#
# Used by CI's jobs (each names the setup services it started) and usable locally. Exit codes: 0
# ready; 1 a setup service failed, or the API didn't answer within 60 s.
set -euo pipefail

cd "$(dirname "$0")/.."
API=${API:-http://localhost:3000}

# docker wait (not docker compose wait, which only sees running containers) returns the exit code
# whether the container is still running or has already exited.
for service in "$@"; do
  code=$(docker wait "$(docker compose ps -a -q "$service")")
  if [ "$code" != "0" ]; then
    echo "$service exited with $code"
    exit 1
  fi
done

# The API is the last service to start; wait until it answers (only if this stack runs it).
if [ -n "$(docker compose ps -q api)" ]; then
  timeout 60 bash -c "until curl -sf -o /dev/null $API/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/balance; do sleep 1; done" ||
    { echo "The API didn't answer at $API within 60 s"; exit 1; }
fi
