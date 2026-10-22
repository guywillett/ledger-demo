#!/usr/bin/env bash
# Exercises the running system the way slice 1's "Done when" list describes, then reconciles.
#
#   docker compose up -d --build && scripts/demo.sh
#
# 1. the same notification 5 times          -> one payment, notification_count 5
# 2. 10 concurrent identical notifications  -> one payment
# 3. the same network ID, another amount    -> 409, nothing changed
# 4. a valid IBAN that isn't ours           -> 422, nothing stored
# 5. a burst of payments, with the worker killed (SIGKILL) mid-review and restarted
# 6. a burst of payments shared by two workers
# then waits until every payment has finished and reconciles: exit 0 only if the books agree
# and nothing is stuck.
#
# The final check covers the whole development database, not just this run: a payment left
# unfinished earlier (an aborted run, a notification sent while the worker was down) fails it
# too, and the reconciliation report lists it. For a clean slate: docker compose down -v.
#
# Uses only the HTTP API and docker compose (reconciliation runs in its own compose service,
# which reads the shared database), so it works against any implementation of the contract.
# Payments go to the demo accounts (alice, bob, carol) in the development database.
#
# Exit codes, as for reconciliation: 0 all good; 1 an expectation failed or the books disagree;
# 2 payments still unfinished after the wait; 3 couldn't run (the API isn't up).
set -euo pipefail

cd "$(dirname "$0")/.."
API=${API:-http://localhost:3000}
ALICE_IBAN=FR7612345000010000000000193
BOB_IBAN=FR7612345000010000000000290
CAROL_IBAN=FR7612345000010000000000387
FOREIGN_IBAN=DE75512108001245126199 # valid, but not one of ours
RUN="demo-$(date +%s)"
failures=0

say() { printf '\n== %s\n' "$*"; }

body() {
  printf '{"network_transaction_id":"%s","creditor_iban":"%s","amount_minor":"%s","currency":"EUR","network":"SEPA"}' "$1" "$2" "$3"
}

# post <network_transaction_id> <iban> <amount_minor>: prints the HTTP status code.
post() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$API/payments" \
    -H 'content-type: application/json' -d "$(body "$1" "$2" "$3")" || true
}

# post_full <network_transaction_id> <iban> <amount_minor>: prints the response body, then
# the HTTP status code on a line of its own. Split with ${x%$'\n'*} (body) and ${x##*$'\n'}
# (status).
post_full() {
  curl -s -w '\n%{http_code}' -X POST "$API/payments" \
    -H 'content-type: application/json' -d "$(body "$1" "$2" "$3")" || true
}

# contains <what> <needle> <haystack>
contains() {
  case "$3" in
    *"$2"*) echo "   ok: $1 ($2)" ;;
    *)
      echo "   FAILED: $1: expected $2 in: $3"
      failures=$((failures + 1))
      ;;
  esac
}

# expect <what> <expected> <actual>
expect() {
  if [ "$2" = "$3" ]; then
    echo "   ok: $1 ($3)"
  else
    echo "   FAILED: $1: expected $2, got $3"
    failures=$((failures + 1))
  fi
}

reconcile() {
  docker compose run --rm --no-deps reconcile "$@"
}

curl -sf -o /dev/null "$API/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/balance" ||
  { echo "The API isn't answering at $API: run docker compose up -d --build first."; exit 3; }

say "1. The same notification 5 times"
codes=""
for _ in 1 2 3 4; do codes="$codes $(post "$RUN-repeat" "$ALICE_IBAN" 1000)"; done
last=$(post_full "$RUN-repeat" "$ALICE_IBAN" 1000)
codes="$codes ${last##*$'\n'}"
expect "one 201, then 200s" " 201 200 200 200 200" "$codes"
# The API's JSON has no spaces, so substring checks are enough (no jq needed).
contains "all five counted on the one payment" '"notification_count":5' "${last%$'\n'*}"

say "2. 10 concurrent identical notifications"
tmp=$(mktemp -d)
# One status code per line (curl's -w output has no newline of its own).
for i in $(seq 1 10); do { post "$RUN-concurrent" "$BOB_IBAN" 2000; echo; } > "$tmp/$i" & done
wait
created=$(cat "$tmp"/* | grep -c '^201$' || true)
duplicates=$(cat "$tmp"/* | grep -c '^200$' || true)
rm -rf "$tmp"
expect "exactly one created" 1 "$created"
expect "the other nine are duplicates" 9 "$duplicates"

say "3. The same network ID with another amount"
expect "conflict" 409 "$(post "$RUN-repeat" "$ALICE_IBAN" 9999)"
# The conflict changed nothing: the next identical repeat is the 6th, with the original amount.
after=$(post_full "$RUN-repeat" "$ALICE_IBAN" 1000)
expect "a repeat after the conflict is still a duplicate" 200 "${after##*$'\n'}"
contains "the conflicting notification wasn't counted" '"notification_count":6' "${after%$'\n'*}"
contains "the stored amount is unchanged" '"amount_minor":"1000"' "${after%$'\n'*}"

say "4. A valid IBAN that isn't one of our accounts"
expect "unknown account" 422 "$(post "$RUN-unknown" "$FOREIGN_IBAN" 500)"

say "5. A burst of payments while the worker is killed mid-review"
for i in $(seq 1 10); do post "$RUN-crash-$i" "$CAROL_IBAN" $((100 * i)) > /dev/null; done
# Let the worker book them and start reviewing. The kill lands mid-review only because the
# review takes REVIEW_DELAY_MS (2000 in compose): with a much shorter delay the payments would
# finish first and this step would test nothing (the run would still pass).
sleep 1
docker compose kill -s SIGKILL worker > /dev/null 2>&1
echo "   worker killed (SIGKILL): unacknowledged messages are redelivered after ack_wait (10 s)"
sleep 2
docker compose up -d worker > /dev/null 2>&1
echo "   worker restarted"

say "6. A burst of payments shared by two workers"
# Back to one worker however the script ends (set -e can stop it anywhere from here).
trap 'docker compose up -d --scale worker=1 worker > /dev/null 2>&1' EXIT
docker compose up -d --scale worker=2 --no-recreate worker > /dev/null 2>&1
for i in $(seq 1 20); do post "$RUN-scaled-$i" "$BOB_IBAN" $((10 * i)) > /dev/null; done
echo "   20 payments posted to two workers"

say "Waiting for every payment to finish (up to 120 s)"
deadline=$((SECONDS + 120))
status=2
while [ "$SECONDS" -lt "$deadline" ]; do
  # 0: consistent and nothing stuck; 2: consistent but payments still in progress; 1: books disagree
  set +e
  reconcile --stuck-after 0 --fail-on-stuck > /dev/null 2>&1
  status=$?
  set -e
  [ "$status" -ne 2 ] && break
  sleep 3
done

say "Reconciliation"
echo "   (deliveries left in_progress are the trace of the killed worker: each redelivery got its own row)"
set +e
reconcile --stuck-after 0 --fail-on-stuck
status=$?
set -e

say "alice's balance"
curl -s "$API/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/balance"
echo

if [ "$failures" -ne 0 ]; then
  echo; echo "$failures expectation(s) failed."; exit 1
fi
case "$status" in
  0) echo; echo "Every payment finished and the books agree." ;;
  2) echo; echo "Some payments did not finish within the wait."; exit 2 ;;
  *) echo; echo "The books disagree."; exit 1 ;;
esac
