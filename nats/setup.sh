#!/bin/sh
# Applies the JetStream definitions in /nats to the NATS server at $NATS_URL. Run by the
# nats-setup compose service on every `docker compose up`, like the migrations: language-
# neutral, so the Node and Go implementations share one definition.
#
#   /nats/streams/<STREAM>.json               one file per stream
#   /nats/consumers/<STREAM>/<CONSUMER>.json  one file per durable consumer on that stream
#
# Streams: `stream add` creates the stream, and succeeds without change if an identical one
# exists. If it exists with a different definition, the server answers error 10058 and
# `stream edit` applies the changes; changes the server refuses (e.g. switching storage type)
# fail loudly. Any other `add` error (unreachable server, invalid definition) is shown as-is.
#
# Consumers: `consumer add` creates or updates the consumer in place. Changes the server
# refuses (e.g. deliver_policy, error 10012) fail loudly; delete the consumer to change them.
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

for config in /nats/consumers/*/*.json; do
  [ -e "$config" ] || continue
  stream=$(basename "$(dirname "$config")")
  name=$(basename "$config" .json)
  if output=$(nats -s "$NATS_URL" consumer add "$stream" --config "$config" 2>&1); then
    echo "consumer $stream/$name: created or updated"
  else
    echo "consumer $stream/$name: $output" >&2
    exit 1
  fi
done
