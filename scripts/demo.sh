#!/usr/bin/env bash
# Exercises the running system the way slice 1's "Done when" list describes, then reconciles.
#
#   docker compose up -d --build && scripts/demo.sh
#
# The provider webhook answers 204 for every notification it handles (the webhook contract), so
# steps 1-4 check the effect through the account's deposits list, not the status code:
# 1. the same notification 5 times          -> one deposit, notification_count 5
# 2. 10 concurrent identical notifications  -> one deposit
# 3. the same network ID, another amount    -> recorded as a conflict, the deposit unchanged
# 4. a valid IBAN that isn't ours           -> recorded in suspense, in no account's list
# 5. a burst of deposits, with the deposit-worker killed (SIGKILL) mid-review and restarted
# 6. a burst of deposits shared by two workers
# then waits until every deposit has finished, funds carol, and runs withdrawals through the
# submitter and provider-sim (both in compose), checking each through the API and her balance:
# 7. a withdrawal                              -> 201 with Location, ends executed
# 8. the same request with the same key        -> 200, the same withdrawal, no second hold
# 9. the same key with another amount          -> 409
# 10. more than available                      -> 422, nothing stored
# 11. SIM FAIL (the provider fails it)          -> ends failed, funds back in available
# 12. SIM EARLY (the result overtakes us)       -> ends executed, applied once
# 13. SIM SLOW with the submitter killed mid-call and restarted -> ends executed, applied once,
#     and the restarted submitter makes no second call
# and finally reconciles: exit 0 only if the books agree and nothing is stuck (deposits,
# withdrawals held or submitted, unpublished events).
#
# The final check covers the whole development database, not just this run: a deposit left
# unfinished earlier (an aborted run, a notification sent while the deposit-worker was down) fails
# it too, and the reconciliation report lists it. For a clean slate: docker compose down -v.
#
# Uses only the HTTP API, provider-sim's test-only endpoint (GET /_test/transfers, slice 4
# decision 73) and docker compose (reconciliation runs in its own compose service, which reads the
# shared database; the submitter's log, for what it did on a redelivery), so it works against any
# implementation of the contract.
# Deposits go to the demo accounts (alice, bob, carol) in the development database: their IDs are
# the seed's, their IBANs read from the API.
#
# Exit codes, as for reconciliation: 0 all good; 1 an expectation failed or the books disagree;
# 2 deposits still unfinished after the wait, or carol couldn't be funded; 3 couldn't run (the API
# isn't up).
set -euo pipefail

cd "$(dirname "$0")/.."
API=${API:-http://localhost:3000}
SIM=${SIM:-http://localhost:4000} # provider-sim
ALICE=acct_01m564xr00fx7rjvqryn5dzwxx
BOB=acct_01m564xr01ecrvnckn84evqsax
CAROL=acct_01m564xr02e70a25p2hkva3gbn
FOREIGN_IBAN=DE75512108001245126199 # valid, but not one of ours
RUN="demo-$(date +%s)"
failures=0

say() { printf '\n== %s\n' "$*"; }

body() {
  printf '{"network_transaction_id":"%s","creditor_iban":"%s","amount_minor":"%s","currency":"EUR","network":"SEPA"}' "$1" "$2" "$3"
}

# post <network_transaction_id> <iban> <amount_minor>: sends the notification to the provider
# webhook and prints the HTTP status code.
post() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$API/provider/incoming-payments" \
    -H 'content-type: application/json' -d "$(body "$1" "$2" "$3")" || true
}

# deposit <account> <network_transaction_id>: prints the account's deposits with that network ID
# (the list's filter: one deposit's JSON, or nothing). The API's JSON has no spaces and a deposit
# object no nested braces, so grep finds each (no jq needed), and not the list's envelope. The
# demo's network IDs need no URL encoding.
deposit() {
  curl -s "$API/accounts/$1/deposits?network_transaction_id=$2" |
    grep -o "{[^{}]*\"network_transaction_id\":\"$2\"[^{}]*}" || true
}

# field <json> <name>: prints a field's value from flat JSON (strings and numbers; first match).
field() {
  printf '%s' "$1" | grep -o "\"$2\":\"\{0,1\}[^\",}]*" | head -1 | sed 's/.*:"\{0,1\}//' || true
}

# balance <account> <bucket>: prints the bucket (pending_in, available, held) in minor units.
balance() {
  field "$(curl -s "$API/accounts/$1/balance")" "$2"
}

# withdraw <account> <key> <amount_minor> <remittance_info>: requests a withdrawal (to a
# foreign IBAN) and prints the response body, then the HTTP status code on a line of its own
# (split with ${x%$'\n'*} and ${x##*$'\n'}). The response headers are left in $headers.
headers=$(mktemp)
withdraw() {
  curl -s -D "$headers" -w '\n%{http_code}' -X POST "$API/accounts/$1/withdrawals" \
    -H 'content-type: application/json' -H "Idempotency-Key: $2" \
    -d "{\"amount_minor\":\"$3\",\"currency\":\"EUR\",\"creditor_iban\":\"$FOREIGN_IBAN\",\"creditor_name\":\"Demo Payee\",\"remittance_info\":\"$4\"}" ||
    true
}

# wait_final <withdrawal id> [timeout s]: polls the withdrawal until it is executed or failed
# (30 s by default) and prints its body. Every withdrawal of the demo's is carol's.
wait_final() {
  local deadline=$((SECONDS + ${2:-30})) body=''
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(curl -s "$API/accounts/$CAROL/withdrawals/$1")
    case "$(field "$body" status)" in executed | failed) break ;; esac
    sleep 1
  done
  printf '%s' "$body"
}

# sim_seen <withdrawal id>: what provider-sim has seen for the withdrawal (its reference), from
# its test-only endpoint: {"reference", "requests", "transfer": null or {…}, …}, or a 404's body.
sim_seen() {
  curl -s "$SIM/_test/transfers?reference=$1" || true
}

# wait_transfer_created <withdrawal id>: waits (20 s at most) until provider-sim has created the
# transfer for this withdrawal, i.e. the submitter's call has reached it.
wait_transfer_created() {
  local deadline=$((SECONDS + 20))
  while [ "$SECONDS" -lt "$deadline" ]; do
    case "$(sim_seen "$1")" in *'"transfer":{'*) return 0 ;; esac
    sleep 0.5
  done
  return 1
}

# submitter_found_executed <withdrawal id>: waits (60 s at most: ack_wait, 30 s, and more) until
# the submitter has logged finding the withdrawal executed (the line with its withdrawal_id and
# status executed), and prints that line's delivery_attempt; nothing if it never did. By log
# fields, the same in both implementations, not by message text.
submitter_found_executed() {
  local deadline=$((SECONDS + 60)) line
  while [ "$SECONDS" -lt "$deadline" ]; do
    line=$(docker compose logs --no-log-prefix submitter 2>/dev/null |
      grep "\"withdrawal_id\":\"$1\"" | grep '"status":"executed"' | head -1 || true)
    if [ -n "$line" ]; then
      printf '%s' "$line" | grep -o '"delivery_attempt":[0-9]*' | sed 's/.*://'
      return 0
    fi
    sleep 1
  done
}

# provider_calls <withdrawal id>: how many transfer requests provider-sim has handled for the
# withdrawal (its reference), from its test-only endpoint. Its whole history only while it is the
# same container: its state is in memory.
provider_calls() {
  field "$(sim_seen "$1")" requests
}

# unfinished <account>: prints the first non-final status any of the account's deposits is in
# (the list's status filter, one status at a time), or nothing.
unfinished() {
  local s
  for s in created received under_review approved rejected; do
    case "$(curl -s "$API/accounts/$1/deposits?status=$s&limit=1")" in
      *'"data":[]'*) ;;
      *) printf '%s' "$s"; return 0 ;;
    esac
  done
}

# fund <account> <iban> <minimum>: posts deposits of 2000 to the account, each waited on until the
# deposit-worker has settled it, until available reaches the minimum. The pseudo review rejects some
# deposits at random, so how many it takes varies; gives up (exit 2) after 20.
fund() {
  local i ntx deadline settled
  for i in $(seq 1 20); do
    [ "$(balance "$1" available)" -ge "$3" ] && return 0
    ntx="$RUN-fund-$i"
    post "$ntx" "$2" 2000 > /dev/null
    deadline=$((SECONDS + 30))
    settled=''
    while [ "$SECONDS" -lt "$deadline" ]; do
      case "$(field "$(deposit "$1" "$ntx")" status)" in credited | returned)
        settled=yes
        break
        ;;
      esac
      sleep 1
    done
    [ -n "$settled" ] || { echo "   deposit $ntx did not settle within 30 s"; exit 2; }
  done
  [ "$(balance "$1" available)" -ge "$3" ] || { echo "   couldn't fund $1 to $3"; exit 2; }
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

curl -sf -o /dev/null "$API/health" ||
  { echo "The API isn't ready at $API: run docker compose up -d --build first."; exit 3; }

# The seed accounts' IBANs, which the notifications are addressed to, from the API.
ALICE_IBAN=$(field "$(curl -s "$API/accounts/$ALICE")" iban)
BOB_IBAN=$(field "$(curl -s "$API/accounts/$BOB")" iban)
CAROL_IBAN=$(field "$(curl -s "$API/accounts/$CAROL")" iban)
[ -n "$ALICE_IBAN" ] && [ -n "$BOB_IBAN" ] && [ -n "$CAROL_IBAN" ] ||
  { echo "Couldn't read the demo accounts' IBANs from $API/accounts/{id}."; exit 3; }

say "1. The same notification 5 times"
codes=""
for _ in 1 2 3 4 5; do codes="$codes $(post "$RUN-repeat" "$ALICE_IBAN" 1000)"; done
expect "five 204s" " 204 204 204 204 204" "$codes"
contains "all five counted on the one deposit" '"notification_count":5' \
  "$(deposit "$ALICE" "$RUN-repeat")"

say "2. 10 concurrent identical notifications"
tmp=$(mktemp -d)
# One status code per line (curl's -w output has no newline of its own).
for i in $(seq 1 10); do { post "$RUN-concurrent" "$BOB_IBAN" 2000; echo; } > "$tmp/$i" & done
wait
handled=$(cat "$tmp"/* | grep -c '^204$' || true)
rm -rf "$tmp"
expect "ten 204s" 10 "$handled"
concurrent=$(deposit "$BOB" "$RUN-concurrent")
expect "exactly one deposit" 1 "$(printf '%s' "$concurrent" | grep -c 'network_transaction_id' || true)"
contains "all ten counted on it" '"notification_count":10' "$concurrent"

say "3. The same network ID with another amount"
expect "recorded, not refused" 204 "$(post "$RUN-repeat" "$ALICE_IBAN" 9999)"
# The conflict changed nothing: the deposit keeps its amount and its count.
after=$(deposit "$ALICE" "$RUN-repeat")
contains "the stored amount is unchanged" '"amount_minor":"1000"' "$after"
contains "the conflicting notification wasn't counted" '"notification_count":5' "$after"

say "4. A valid IBAN that isn't one of our accounts"
expect "recorded, not refused" 204 "$(post "$RUN-unknown" "$FOREIGN_IBAN" 500)"
for account in "$ALICE" "$BOB" "$CAROL"; do
  expect "not in $account's deposits" "" "$(deposit "$account" "$RUN-unknown")"
done
# Recorded in suspense instead: the whole-database reconciliation report lists it.
contains "listed in the reconciliation report's suspense section" "$RUN-unknown" \
  "$(reconcile --stuck-after 10 2>/dev/null || true)"

say "5. A burst of deposits while the deposit-worker is killed mid-review"
for i in $(seq 1 10); do post "$RUN-crash-$i" "$CAROL_IBAN" $((100 * i)) > /dev/null; done
# Let the deposit-worker book them and start reviewing. The kill lands mid-review only because the
# review takes REVIEW_DELAY_MS (2000 in compose): with a much shorter delay the deposits would
# finish first and this step would test nothing (the run would still pass).
sleep 1
docker compose kill -s SIGKILL deposit-worker > /dev/null 2>&1
echo "   deposit-worker killed (SIGKILL): unacknowledged messages are redelivered after ack_wait (10 s)"
sleep 2
# --no-deps here and on every up below: start this one service, never its dependencies. Without it
# compose may recreate a dependency, and provider-sim keeps its transfers in memory (step 13). It
# does so after an up --build whose parallel builds of the shared ledger-demo-node image leave
# each container's com.docker.compose.image label naming a build other than the tagged one: the
# next up sees a changed image (seen with Compose v2.18).
docker compose up -d --no-deps deposit-worker > /dev/null 2>&1
echo "   deposit-worker restarted"

say "6. A burst of deposits shared by two workers"
# Back to one deposit-worker however the script ends (set -e can stop it anywhere from here).
trap 'docker compose up -d --no-deps --scale deposit-worker=1 deposit-worker > /dev/null 2>&1' EXIT
docker compose up -d --no-deps --scale deposit-worker=2 --no-recreate deposit-worker > /dev/null 2>&1
for i in $(seq 1 20); do post "$RUN-scaled-$i" "$BOB_IBAN" $((10 * i)) > /dev/null; done
echo "   20 deposits posted to two workers"

say "Waiting for every deposit to finish (up to 120 s)"
# Through the deposits lists' status filter, every demo account (every deposit of a run, earlier
# runs' included, is one of theirs; suspense deposits are final). The final reconciliation checks
# the rest (the outbox, the books).
deadline=$((SECONDS + 120))
while [ "$SECONDS" -lt "$deadline" ]; do
  pending=""
  for account in "$ALICE" "$BOB" "$CAROL"; do
    pending=$(unfinished "$account")
    [ -n "$pending" ] && break
  done
  [ -z "$pending" ] && break
  sleep 1
done
[ -z "$pending" ] && echo "   all finished" || echo "   a deposit is still $pending (the final reconciliation reports it)"

say "Funding carol for the withdrawals (deposits of 2000 until available >= 10000)"
fund "$CAROL" "$CAROL_IBAN" 10000
echo "   carol's available: $(balance "$CAROL" available)"

say "7. A withdrawal"
before=$(balance "$CAROL" available)
response=$(withdraw "$CAROL" "$RUN-wd-1" 700 "Rent October")
expect "created" 201 "${response##*$'\n'}"
withdrawal=$(field "${response%$'\n'*}" id)
# Header names are case-insensitive (RFC 9110 5.1): Node sends "location", Go "Location".
# Lowercased whole (TypeIDs are lowercase already).
contains "with its Location" "location: /accounts/$CAROL/withdrawals/$withdrawal" "$(tr -d '\r' < "$headers" | tr 'A-Z' 'a-z')"
expect "ends executed" executed "$(field "$(wait_final "$withdrawal")" status)"
expect "available down by the amount" $((before - 700)) "$(balance "$CAROL" available)"
expect "nothing left held" 0 "$(balance "$CAROL" held)"

say "8. The same request again, with the same key"
before=$(balance "$CAROL" available)
response=$(withdraw "$CAROL" "$RUN-wd-1" 700 "Rent October")
expect "a repeat" 200 "${response##*$'\n'}"
expect "the same withdrawal" "$withdrawal" "$(field "${response%$'\n'*}" id)"
expect "no second hold" "$before/0" "$(balance "$CAROL" available)/$(balance "$CAROL" held)"

say "9. The same key with another amount"
expect "key reused with other details" 409 "$(withdraw "$CAROL" "$RUN-wd-1" 701 "Rent October" | tail -1)"

say "10. More than available"
before=$(balance "$CAROL" available)
expect "insufficient funds" 422 "$(withdraw "$CAROL" "$RUN-wd-2" $((before + 1)) "Too much" | tail -1)"
expect "nothing stored or held" "$before/0" "$(balance "$CAROL" available)/$(balance "$CAROL" held)"

say "11. The provider fails the transfer (SIM FAIL)"
before=$(balance "$CAROL" available)
response=$(withdraw "$CAROL" "$RUN-wd-3" 500 "SIM FAIL")
expect "created" 201 "${response##*$'\n'}"
expect "ends failed" failed "$(field "$(wait_final "$(field "${response%$'\n'*}" id)")" status)"
expect "funds back in available" "$before/0" "$(balance "$CAROL" available)/$(balance "$CAROL" held)"

say "12. The provider's result arrives before its answer to the submitter (SIM EARLY)"
before=$(balance "$CAROL" available)
response=$(withdraw "$CAROL" "$RUN-wd-4" 300 "SIM EARLY")
expect "created" 201 "${response##*$'\n'}"
expect "ends executed" executed "$(field "$(wait_final "$(field "${response%$'\n'*}" id)")" status)"
expect "applied once" "$((before - 300))/0" "$(balance "$CAROL" available)/$(balance "$CAROL" held)"

say "13. The submitter killed during a slow provider call (SIM SLOW), then restarted"
# provider-sim creates the transfer but answers slowly, and the submitter is killed while it
# waits: our side never learns the outcome. What this step shows: the provider's result reaches
# the webhook (after SIM_EXECUTION_DELAY_MS, 2 s) before the killed submitter's message is
# redelivered (after ack_wait, 30 s), so the withdrawal is executed while the submitter is down,
# and the restarted submitter finds it executed and makes no call. Nothing is lost and nothing is
# applied twice. Checked: the restarted submitter's first delivery (attempt 2, after the killed
# attempt 1) finds it executed, so it made no call; and provider-sim, the same container
# throughout (so its state is its whole history), handled one request for it. The same-key retry
# of an unknown outcome is proven elsewhere: test/submitter/provider.test.ts ("maps a timeout to
# unknown, and the next call with the same key gets the same, single transfer") and
# test/e2e/submitter.e2e.ts ("retries a 503 with the same key and executes it (SIM ERROR)").
#
# The kill waits for provider-sim to have created the transfer, rather than a fixed delay: on a
# slow machine the submitter may not have made the call yet, and killing it before the call shows
# something else (a redelivery after ack_wait). wait_final allows more than ack_wait anyway.
#
# provider-sim's request count is in memory (a known gap): a restart would forget the first call
# and make "one call" pass whatever happened. So the check also requires the same container
# throughout (its ID before and after).
before=$(balance "$CAROL" available)
provider_sim=$(docker compose ps -q provider-sim)
response=$(withdraw "$CAROL" "$RUN-wd-5" 400 "SIM SLOW")
expect "created" 201 "${response##*$'\n'}"
withdrawal=$(field "${response%$'\n'*}" id)
if wait_transfer_created "$withdrawal"; then
  echo "   the provider has created the transfer and is answering slowly"
else
  echo "   FAILED: the provider never received the transfer"
  failures=$((failures + 1))
fi
docker compose kill -s SIGKILL submitter > /dev/null 2>&1
echo "   submitter killed (SIGKILL) during the call"
docker compose up -d --no-deps submitter > /dev/null 2>&1
echo "   submitter restarted"
expect "ends executed" executed "$(field "$(wait_final "$withdrawal" 60)" status)"
expect "applied once" "$((before - 400))/0" "$(balance "$CAROL" available)/$(balance "$CAROL" held)"
expect "the restarted submitter's first delivery found it executed (attempt)" 2 \
  "$(submitter_found_executed "$withdrawal")"
expect "provider-sim kept its state (the same container)" "$provider_sim" \
  "$(docker compose ps -q provider-sim)"
expect "one call to the provider" 1 "$(provider_calls "$withdrawal")"

say "Reconciliation"
echo "   (deliveries left in_progress are the trace of the killed deposit-worker and submitter: each redelivery got its own row)"
set +e
reconcile --stuck-after 0 --fail-on-stuck
status=$?
set -e

say "carol's balance"
curl -s "$API/accounts/$CAROL/balance"
echo
rm -f "$headers"

if [ "$failures" -ne 0 ]; then
  echo; echo "$failures expectation(s) failed."; exit 1
fi
case "$status" in
  0) echo; echo "Every deposit and withdrawal finished and the books agree." ;;
  2) echo; echo "Some deposits or withdrawals did not finish within the wait."; exit 2 ;;
  *) echo; echo "The books disagree."; exit 1 ;;
esac
