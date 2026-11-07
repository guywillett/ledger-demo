#!/usr/bin/env bash
# A Node and a Go deposit-worker sharing one durable consumer (slice 3, step 8a): each message goes
# to one of them, both process some, none is processed twice, and the books still agree.
#
#   docker compose up -d --build && scripts/shared-consumer.sh
#
# Starts deposit-worker-other (the compose profile "shared-consumer") in the implementation
# opposite to the running deposit-worker's (DEPOSIT_WORKER_IMPL, node by default), on the same
# durable consumer; posts a burst of deposits; waits until each has finished; then checks from the
# two workers' logs, by their "implementation" field, that both processed some deposits and no
# deposit was processed twice; and finally reconciles. Stops deposit-worker-other however it ends
# (the stack is left as it was found).
#
# Exit codes, as demo.sh's: 0 all good; 1 an expectation failed or the books disagree; 2 deposits
# still unfinished after the wait; 3 couldn't run (the API isn't up).
set -euo pipefail

cd "$(dirname "$0")/.."
API=${API:-http://localhost:3000}
ALICE=acct_01m564xr00fx7rjvqryn5dzwxx
BOB=acct_01m564xr01ecrvnckn84evqsax
CAROL=acct_01m564xr02e70a25p2hkva3gbn
RUN="shared-$(date +%s)"
DEPOSITS=30
failures=0

main_impl=${DEPOSIT_WORKER_IMPL:-node}
case "$main_impl" in node) other_impl=go ;; go) other_impl=node ;; *)
  echo "DEPOSIT_WORKER_IMPL must be node or go, not $main_impl"; exit 3 ;;
esac
export DEPOSIT_WORKER_OTHER_IMPL=$other_impl

say() { printf '\n== %s\n' "$*"; }

post() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$API/provider/incoming-payments" \
    -H 'content-type: application/json' \
    -d "{\"network_transaction_id\":\"$1\",\"creditor_iban\":\"$2\",\"amount_minor\":\"$3\",\"currency\":\"EUR\",\"network\":\"SEPA\"}" ||
    true
}

# deposit <account> <network_transaction_id>: the deposit's JSON from the account's deposits with
# that network ID (the list's filter), or nothing (as demo.sh's: the API's JSON has no spaces and a
# deposit object no nested braces).
deposit() {
  curl -s "$API/accounts/$1/deposits?network_transaction_id=$2" |
    grep -o "{[^{}]*\"network_transaction_id\":\"$2\"[^{}]*}" || true
}

field() {
  printf '%s' "$1" | grep -o "\"$2\":\"\{0,1\}[^\",}]*" | head -1 | sed 's/.*:"\{0,1\}//' || true
}

expect() {
  if [ "$2" = "$3" ]; then
    echo "   ok: $1 ($3)"
  else
    echo "   FAILED: $1: expected $2, got $3"
    failures=$((failures + 1))
  fi
}

curl -sf -o /dev/null "$API/health" ||
  { echo "The API isn't ready at $API: run docker compose up -d --build first."; exit 3; }

# The seed accounts' IBANs, which the notifications are addressed to, from the API.
ALICE_IBAN=$(field "$(curl -s "$API/accounts/$ALICE")" iban)
BOB_IBAN=$(field "$(curl -s "$API/accounts/$BOB")" iban)
CAROL_IBAN=$(field "$(curl -s "$API/accounts/$CAROL")" iban)
[ -n "$ALICE_IBAN" ] && [ -n "$BOB_IBAN" ] && [ -n "$CAROL_IBAN" ] ||
  { echo "Couldn't read the demo accounts' IBANs from $API/accounts/{id}."; exit 3; }

say "Starting a $other_impl deposit-worker beside the $main_impl one, on the same consumer"
# However the script ends, stop and remove it again: the stack is left as it was found.
trap 'docker compose --profile shared-consumer rm -sf deposit-worker-other > /dev/null 2>&1' EXIT
docker compose --profile shared-consumer up -d --no-deps deposit-worker-other > /dev/null 2>&1
deadline=$((SECONDS + 60))
until docker compose logs deposit-worker-other 2>/dev/null | grep -q '"msg":"deposit-worker started"'; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "   deposit-worker-other didn't start"; exit 3; }
  sleep 1
done
echo "   started"

say "A burst of $DEPOSITS deposits"
accounts=("$ALICE" "$BOB" "$CAROL")
ibans=("$ALICE_IBAN" "$BOB_IBAN" "$CAROL_IBAN")
for i in $(seq 1 "$DEPOSITS"); do
  post "$RUN-$i" "${ibans[$((i % 3))]}" $((100 + i)) > /dev/null
done
echo "   posted"

say "Waiting for every deposit to finish (up to 120 s)"
deadline=$((SECONDS + 120))
ids=()
while :; do
  ids=()
  for i in $(seq 1 "$DEPOSITS"); do
    d=$(deposit "${accounts[$((i % 3))]}" "$RUN-$i")
    case "$(field "$d" status)" in credited | returned) ids+=("$(field "$d" id)") ;; esac
  done
  [ "${#ids[@]}" -eq "$DEPOSITS" ] && break
  [ "$SECONDS" -lt "$deadline" ] || { echo "   only ${#ids[@]} of $DEPOSITS finished"; exit 2; }
  sleep 2
done
echo "   all $DEPOSITS finished"

say "Who processed them"
# Every "deposit processed" line of both workers, as "<implementation> <deposit_id>". The sed
# relies on "implementation" coming before "deposit_id" on a line, true of both: pino writes its
# base fields first, and slog writes attributes in the order they were added (implementation at
# start-up, deposit_id later, by the delivery's logger).
#
# "deposit processed" is logged only when a delivery made a transition; a redelivery that finds
# everything done logs "duplicate delivery" instead. So "none processed twice" checks effects, not
# deliveries, and holds under at-least-once delivery. (A redelivery after a partial run, a crash
# between transactions, would rightly log a second line with the remaining transitions: this burst
# provokes none.)
processed=$(docker compose --profile shared-consumer logs --no-log-prefix deposit-worker deposit-worker-other 2>/dev/null |
  grep '"msg":"deposit processed"' |
  sed -n 's/.*"implementation":"\([a-z]*\)".*"deposit_id":"\(dep_[0-9a-z]*\)".*/\1 \2/p')
done_count=0
twice=0
by_node=0
by_go=0
for id in "${ids[@]}"; do
  lines=$(printf '%s\n' "$processed" | grep -c " $id\$" || true)
  [ "$lines" -gt 0 ] && done_count=$((done_count + 1))
  [ "$lines" -gt 1 ] && twice=$((twice + 1))
  printf '%s\n' "$processed" | grep -q "^node $id\$" && by_node=$((by_node + 1))
  printf '%s\n' "$processed" | grep -q "^go $id\$" && by_go=$((by_go + 1))
done
echo "   node processed $by_node, go processed $by_go (of $DEPOSITS)"
expect "every deposit processed" "$DEPOSITS" "$done_count"
expect "none processed twice" 0 "$twice"
[ "$by_node" -gt 0 ] && echo "   ok: the Node worker processed some" ||
  { echo "   FAILED: the Node worker processed none"; failures=$((failures + 1)); }
[ "$by_go" -gt 0 ] && echo "   ok: the Go worker processed some" ||
  { echo "   FAILED: the Go worker processed none"; failures=$((failures + 1)); }

say "Reconciliation"
set +e
docker compose run --rm --no-deps reconcile --stuck-after 0 --fail-on-stuck
status=$?
set -e

if [ "$failures" -ne 0 ]; then
  echo; echo "$failures expectation(s) failed."; exit 1
fi
case "$status" in
  0) echo; echo "Both implementations shared the consumer, each deposit once, and the books agree." ;;
  2) echo; echo "Something did not finish."; exit 2 ;;
  *) echo; echo "The books disagree."; exit 1 ;;
esac
