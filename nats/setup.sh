#!/bin/sh
# Applies every stream definition in /nats/streams (one file per stream, named after it) to the
# NATS server at $NATS_URL. Run by the nats-setup compose service on every `docker compose up`,
# like the migrations: language-neutral, so the Node and Go implementations share one definition.
#
# `stream add` creates the stream, and succeeds without change if an identical one exists. If
# the stream exists with a different definition, the server answers error 10058 and
# `stream edit` applies the changes; changes the server refuses (e.g. switching storage type)
# fail loudly. Any other `add` error (unreachable server, invalid definition) is shown as-is.
#
# Detecting "exists with a different definition" relies on the CLI's error text containing
# "(10058)", which may change between nats-box versions: it works because the image version is
# pinned in docker-compose.yml; re-check it when upgrading. Applying `stream edit --force` on
# every `up` suits a demo; in production, stream changes would be gated and reviewed like
# migrations (some edits, e.g. a shorter max_age, discard stored messages).
set -eu

for config in /nats/streams/*.json; do
  name=$(basename "$config" .json)
  if output=$(nats -s "$NATS_URL" stream add --config "$config" 2>&1); then
    echo "stream $name: created or already up to date"
  elif echo "$output" | grep -q "(10058)"; then
    nats -s "$NATS_URL" stream edit "$name" --config "$config" --force >/dev/null
    echo "stream $name: updated"
  else
    echo "stream $name: $output" >&2
    exit 1
  fi
done
