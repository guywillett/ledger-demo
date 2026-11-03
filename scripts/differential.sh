#!/usr/bin/env bash
# A differential check of the read endpoints (slice 3, step 8b, decision 61): the same requests to a
# Node and a Go API on one database, compared byte for byte.
#
#   docker compose up -d --build && scripts/demo.sh && scripts/differential.sh
#
# Run it after scripts/demo.sh: it creates no data of its own and needs the demo's, deposits and
# withdrawals in every final status, all finished (a deposit or withdrawal changing between the two
# APIs' answers would be a false difference). It exits 3 if it finds too little.
#
# Starts api-other (the compose profile "differential", host port 3001) in the implementation
# opposite to the running api's (API_IMPL, node by default), and sends each request to both:
# the health check, balances, every page of each demo account's transactions (each page's cursor
# used on the other implementation too), the query parameters' edge cases, withdrawals in each final
# status, unknown and malformed IDs, unknown routes and wrong methods, and replays of the demo's
# withdrawal requests (the same key and body, then the same key and another amount: writes that
# change nothing). It compares the status code, the body byte for byte, and the headers: names
# case-insensitively, ignoring date, x-request-id and the transport headers. A difference is
# printed (the request, the headers side by side, a diff of the bodies) and the check carries on.
# Stops api-other however it ends.
#
# Exit codes, as the other scripts': 0 every response the same; 1 a difference, or a request there
# to succeed (a page, a replay) that didn't; 3 couldn't run (an API isn't up, or too little data:
# run scripts/demo.sh first).
set -euo pipefail

cd "$(dirname "$0")/.."
API=${API:-http://localhost:3000}
API_OTHER=${API_OTHER:-http://localhost:3001}
ALICE=acct_01m564xr00fx7rjvqryn5dzwxx
BOB=acct_01m564xr01ecrvnckn84evqsax
CAROL=acct_01m564xr02e70a25p2hkva3gbn
ACCOUNTS="$ALICE $BOB $CAROL"
PAGE=7 # a small page, so each account's list takes several

main_impl=${API_IMPL:-node}
case "$main_impl" in node) other_impl=go ;; go) other_impl=node ;; *)
  echo "API_IMPL must be node or go, not $main_impl"; exit 3 ;;
esac
export API_OTHER_IMPL=$other_impl

work=$(mktemp -d)
# However the script ends, stop and remove api-other again: the stack is left as it was found.
trap 'docker compose --profile differential rm -sf api-other > /dev/null 2>&1; rm -rf "$work"' EXIT

requests=0
differences=0
unexpected=0

say() { printf '\n== %s\n' "$*"; }

too_little() {
  echo "   $*"
  echo "Too little data to compare: run scripts/demo.sh first (on a stack where it passes)."
  exit 3
}

# fetch <base url> <side: a|b> <method> <path> [body] [idempotency key]: the response's head and
# body in $work/<side>.head and $work/<side>.body. --path-as-is and --globoff: the path goes as
# written (no dot-segment removal, no {} or [] expansion).
fetch() {
  local args=(-s --path-as-is --globoff -o "$work/$2.body" -D "$work/$2.head")
  if [ "$3" = HEAD ]; then
    : > "$work/$2.body"
    args=(-s --path-as-is --globoff -I -o "$work/$2.head")
  else
    args+=(-X "$3")
  fi
  [ -n "${5:-}" ] && args+=(-H 'content-type: application/json' --data-binary "$5")
  [ -n "${6:-}" ] && args+=(-H "Idempotency-Key: $6")
  : > "$work/$2.head"
  curl "${args[@]}" "$1$4" || true
}

status_of() { tr -d '\r' < "$work/$1.head" | head -1 | cut -d' ' -f2; }

# The headers compared: every one but the ignored, as "name: value" with the name in lower case
# (header names are case-insensitive; Node writes them in lower case, Go in canonical case), sorted.
headers_of() {
  tr -d '\r' < "$work/$1.head" | sed 1d | awk '
    index($0, ":") > 0 {
      name = tolower(substr($0, 1, index($0, ":") - 1))
      value = substr($0, index($0, ":") + 1)
      sub(/^[ \t]+/, "", value)
      if (name ~ /^(date|x-request-id|content-length|connection|keep-alive|transfer-encoding)$/) next
      print name ": " value
    }' | sort
}

# For reading a JSON body in a diff: a line break after each comma and opening brace. Display
# only; the comparison is of the bytes.
spread() { awk '{ gsub(/,"/, ",\n\""); gsub(/\{/, "{\n"); print }' "$1"; }

# compare <method> <path> [body] [idempotency key]: sends the request to both APIs and compares.
compare() {
  requests=$((requests + 1))
  last_request="$1 $2"
  fetch "$API" a "$@"
  fetch "$API_OTHER" b "$@"
  headers_of a > "$work/a.headers"
  headers_of b > "$work/b.headers"
  if [ "$(status_of a)" = "$(status_of b)" ] && cmp -s "$work/a.headers" "$work/b.headers" &&
    cmp -s "$work/a.body" "$work/b.body"; then
    return 0
  fi
  differences=$((differences + 1))
  echo
  echo "   DIFFERENT: $1 $2${3:+ with body $3}${4:+ (Idempotency-Key $4)}"
  echo "   status: $main_impl $(status_of a), $other_impl $(status_of b)"
  if cmp -s "$work/a.headers" "$work/b.headers"; then
    echo "   headers: the same"
  else
    echo "   headers ($main_impl | $other_impl):"
    diff -y -W 160 "$work/a.headers" "$work/b.headers" | sed 's/^/     /' || true
  fi
  echo "   body:"
  diff -u --label "$main_impl" --label "$other_impl" <(spread "$work/a.body") \
    <(spread "$work/b.body") | sed 's/^/     /' || true
}

# expect_status <code>: after compare, for a request whose comparison means something only if it
# did what it is there for (a replay answered 400 by both would be "the same"): the api's status.
expect_status() {
  local got
  got=$(status_of a)
  [ "$got" = "$1" ] && return 0
  unexpected=$((unexpected + 1))
  echo "   UNEXPECTED: $last_request: $main_impl answered $got, not $1"
}

# field <json> <name>: a string field's value (or null), as demo.sh's helper. Our JSON has no
# spaces; a value with an escaped quote isn't read (none in the demo's data).
field() {
  printf '%s' "$1" | grep -oE "\"$2\":(\"[^\"\\\\]*\"|null)" | head -1 | sed 's/^"[^"]*"://; s/^"//; s/"$//' || true
}

# typeid_suffix <uuid>: the UUID's TypeID suffix (without the prefix): two zero bits, then the 128
# bits, five at a time in Crockford's base32, lower case: 26 characters.
typeid_suffix() {
  printf '%s\n' "$1" | awk '
    BEGIN {
      split("0000 0001 0010 0011 0100 0101 0110 0111 1000 1001 1010 1011 1100 1101 1110 1111", b, " ")
      split("0 1 2 3 4 5 6 7 8 9 a b c d e f", h, " ")
      for (i = 1; i <= 16; i++) nibble[h[i]] = b[i]
      alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
    }
    {
      hex = tolower($1)
      gsub(/-/, "", hex)
      bits = "00"
      for (i = 1; i <= 32; i++) bits = bits nibble[substr(hex, i, 1)]
      out = ""
      for (i = 1; i <= 130; i += 5) {
        n = 0
        for (j = 0; j < 5; j++) n = n * 2 + substr(bits, i + j, 1)
        out = out substr(alphabet, n + 1, 1)
      }
      print out
    }'
}

# A well-formed ID that names nothing: the same prefix and a different last character (the low
# bits of the random part, so still a UUIDv7).
unknown_id() {
  case "$1" in *0) printf '%s1' "${1%?}" ;; *) printf '%s0' "${1%?}" ;; esac
}

curl -sf -o /dev/null "$API/health" ||
  { echo "The API isn't ready at $API: run docker compose up -d --build first."; exit 3; }

say "What there is to compare"
# Every deposit of the demo accounts, read from the api a page of 200 at a time.
statuses=""
deposit_ids=""
multi_page=0
for account in $ACCOUNTS; do
  before=""
  count=0
  while :; do
    body=$(curl -s "$API/accounts/$account/transactions?limit=200${before:+&before=$before}")
    statuses="$statuses $(printf '%s' "$body" | grep -o '"status":"[a-z_]*"' | sed 's/.*:"//; s/"//' | tr '\n' ' ')"
    ids=$(printf '%s' "$body" | grep -o '"id":"dep_[0-9a-z]*"' | sed 's/.*:"//; s/"//' | tr '\n' ' ')
    deposit_ids="$deposit_ids $ids"
    count=$((count + $(printf '%s\n' $ids | grep -c . || true)))
    before=$(field "$body" next_before)
    [ -z "$before" ] || [ "$before" = null ] && break
  done
  [ "$count" -gt "$PAGE" ] && multi_page=$((multi_page + 1))
done
for s in created received under_review approved rejected; do
  case " $statuses " in *" $s "*) too_little "a deposit is still $s: wait until every deposit has finished" ;; esac
done
deposit_statuses=""
for s in credited returned; do
  case " $statuses " in *" $s "*) deposit_statuses="$deposit_statuses $s" ;; *) too_little "no $s deposit" ;; esac
done
[ "$multi_page" -gt 0 ] || too_little "no account has more than $PAGE deposits, so no list takes several pages"

# Withdrawals can't be listed through the API: their IDs are read from the database (as
# reconciliation reads it), which depends on neither API under test. (demo.sh knows its
# withdrawals' IDs from its POST responses; this script, run after it, has no record of them.)
withdrawals=$(docker compose exec -T postgres psql -U ledger -d ledger -Atc \
  "SELECT status, id FROM withdrawals ORDER BY id" 2>/dev/null | tr '|' ' ') ||
  { echo "Couldn't read the withdrawals from the database (is postgres up?)"; exit 3; }
executed=""
failed=""
while read -r status uuid; do
  [ -n "$uuid" ] || continue
  id="wd_$(typeid_suffix "$uuid")"
  case "$status" in
    executed) [ -n "$executed" ] || executed=$id ;;
    failed) [ -n "$failed" ] || failed=$id ;;
    *) too_little "withdrawal $id is still $status: wait until it has finished" ;;
  esac
done <<< "$withdrawals"
[ -n "$executed" ] || too_little "no executed withdrawal"
[ -n "$failed" ] || too_little "no failed withdrawal"
first_deposit=$(printf '%s\n' $deposit_ids | head -1)
echo "   deposits:$deposit_statuses; withdrawals: executed $executed, failed $failed"

say "Starting a $other_impl API beside the $main_impl one, on port ${API_OTHER##*:}"
docker compose --profile differential up -d --no-deps api-other > /dev/null 2>&1
deadline=$((SECONDS + 60))
until curl -sf -o /dev/null "$API_OTHER/health"; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "   api-other isn't ready at $API_OTHER"; exit 3; }
  sleep 1
done
echo "   started"

say "Health"
# Ready only: a 503 needs a database that doesn't answer, which this stack can't arrange.
compare GET /health
expect_status 200
compare HEAD /health

say "Balances"
for account in $ACCOUNTS; do
  compare GET "/accounts/$account/balance"
  expect_status 200
done
compare HEAD "/accounts/$ALICE/balance"
compare GET "/accounts/$(unknown_id "$ALICE")/balance"
for id in nope acct_123 "acct_${ALICE#acct_}x" "$(printf '%s' "$ALICE" | tr 'a-z' 'A-Z')" \
  "acct_8${ALICE#acct_?}" "pay_${ALICE#acct_}" "dep_${ALICE#acct_}" "wd_${ALICE#acct_}" \
  "${ALICE#acct_}" "acct_" "acct%5F${ALICE#acct_}"; do
  compare GET "/accounts/$id/balance"
done

say "Transactions: every page, each cursor used on both implementations"
# walk <account> <query> <first side: a|b>: every page, taking the next page's cursor alternately
# from one implementation's answer and the other's (both receive every page's request).
walk() {
  local before="" side=$3 pages=0
  while [ "$pages" -lt 500 ]; do
    compare GET "/accounts/$1/transactions?$2${before:+&before=$before}"
    expect_status 200
    pages=$((pages + 1))
    before=$(field "$(cat "$work/$side.body")" next_before)
    [ -z "$before" ] || [ "$before" = null ] && return 0
    case "$side" in a) side=b ;; b) side=a ;; esac
  done
}
for account in $ACCOUNTS; do
  for first in a b; do
    walk "$account" "limit=$PAGE" "$first"
    walk "$account" "limit=$PAGE&include=history" "$first"
  done
  walk "$account" "" a
done

say "Transactions: query parameters"
T="/accounts/$ALICE/transactions"
for q in limit=0 limit=1 limit=200 limit=201 limit=abc limit=-1 limit=1.5 limit= limit=01 \
  "limit=1&limit=2" limit=%31 limit=99999999999999999999 \
  before=nope "before=wd_${first_deposit#dep_}" "before=dep_8${first_deposit#dep_?}" \
  "before=$(unknown_id "$first_deposit")" "before=$first_deposit" before= \
  include=history include=nope include= "include=history&include=history" foo=bar \
  "limit=1&foo=bar"; do
  compare GET "$T?$q"
done
compare HEAD "$T?limit=1"
compare GET "/accounts/$(unknown_id "$ALICE")/transactions"
compare GET "/accounts/nope/transactions"
compare GET "/accounts/dep_${ALICE#acct_}/transactions"

say "Withdrawals"
for id in "$executed" "$failed"; do
  compare GET "/withdrawals/$id"
  expect_status 200
  compare GET "/withdrawals/$id?include=history"
  expect_status 200
  compare GET "/withdrawals/$id?include=nope"
  compare GET "/withdrawals/$id?foo=bar"
done
compare HEAD "/withdrawals/$executed"
for id in "$(unknown_id "$executed")" nope wd_123 "dep_${executed#wd_}" "acct_${executed#wd_}" \
  "wd_8${executed#wd_?}" "$(printf '%s' "$executed" | tr 'a-z' 'A-Z')" "${executed#wd_}"; do
  compare GET "/withdrawals/$id"
done

say "Unknown routes and wrong methods"
for path in / /nope /accounts "/accounts/$ALICE" "/accounts/$ALICE/" "/accounts/$ALICE/balance/" \
  "/accounts/$ALICE/nope" /withdrawals /withdrawals/ "/withdrawals/$executed/" \
  "/Accounts/$ALICE/balance" /accounts//balance "//accounts/$ALICE/balance" \
  "/accounts/$ALICE/../$BOB/balance" /provider /provider/nope; do
  compare GET "$path"
done
for m in POST PUT PATCH DELETE OPTIONS; do
  compare "$m" "/accounts/$ALICE/balance"
done
compare POST "/accounts/$ALICE/transactions"
compare DELETE "/withdrawals/$executed"
compare PUT "/withdrawals/$executed"
compare GET "/accounts/$ALICE/withdrawals"
compare PUT "/accounts/$ALICE/withdrawals"
compare GET /provider/incoming-payments
compare GET /provider/transfer-updates
compare DELETE /provider/transfer-updates

say "Replays of the demo's withdrawal requests"
# The request rebuilt from the withdrawal's own fields (the response holds every field of the
# request): the same key and body returns the stored withdrawal (200), the same key with another
# amount the reuse error (409). Neither stores anything.
for id in "$executed" "$failed"; do
  w=$(curl -s "$API/withdrawals/$id")
  account=$(field "$w" account_id)
  key=$(field "$w" idempotency_key)
  amount=$(field "$w" amount_minor)
  remittance=$(field "$w" remittance_info)
  [ "$remittance" = null ] || remittance="\"$remittance\""
  request() {
    printf '{"amount_minor":"%s","currency":"EUR","creditor_iban":"%s","creditor_name":"%s","remittance_info":%s}' \
      "$1" "$(field "$w" creditor_iban)" "$(field "$w" creditor_name)" "$remittance"
  }
  compare POST "/accounts/$account/withdrawals" "$(request "$amount")" "$key"
  expect_status 200
  compare POST "/accounts/$account/withdrawals" "$(request $((amount + 1)))" "$key"
  expect_status 409
done

echo
if [ "$differences" -ne 0 ] || [ "$unexpected" -ne 0 ]; then
  echo "$differences of $requests requests answered differently by $main_impl and $other_impl;" \
    "$unexpected answered other than expected."
  exit 1
fi
echo "All $requests requests answered the same by $main_impl and $other_impl."
