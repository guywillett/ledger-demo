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
# the health check, the accounts, balances, every page of each demo account's deposits (each
# page's cursor used on the other implementation too, filtered and not), the query parameters' and
# the filters' edge cases, single deposits (found, another account's, unknown, malformed IDs),
# the same for withdrawals (every page of each account's list, the status filter, withdrawals in
# each final status), every page of each account's transactions (both kinds' cursors), unknown and malformed IDs, unknown routes and wrong methods,
# and replays of the demo's withdrawal requests (the same key and body, then the same key and
# another amount: writes that change nothing). It compares the status code, the body byte for
# byte, and the headers: names case-insensitively, ignoring date, x-request-id and the transport
# headers. A difference is printed (the request, the headers side by side, a diff of the bodies)
# and the check carries on. Stops api-other however it ends.
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

# A well-formed ID that names nothing: the same prefix and a different last character (the low
# bits of the random part, so still a UUIDv7).
unknown_id() {
  case "$1" in *0) printf '%s1' "${1%?}" ;; *) printf '%s0' "${1%?}" ;; esac
}

curl -sf -o /dev/null "$API/health" ||
  { echo "The API isn't ready at $API: run docker compose up -d --build first."; exit 3; }

# first_id <path>: the ID of the first item in the api's list at the path; nothing if it's empty.
first_id() { field "$(curl -s "$API$1")" id; }

say "What there is to compare"
# Through the lists' status filters, for each demo account: no deposit or withdrawal still in
# progress (one changing between the two APIs' answers would be a false difference), one of each
# final status for the single reads (as "<account> <id>"), and an account whose list takes several
# pages.
credited_deposit=""
returned_deposit=""
executed=""
failed=""
multi_page=0
for account in $ACCOUNTS; do
  for s in created received under_review approved rejected; do
    [ -z "$(first_id "/accounts/$account/deposits?status=$s&limit=1")" ] ||
      too_little "a deposit of $account is still $s: wait until every deposit has finished"
  done
  for s in held submitted; do
    [ -z "$(first_id "/accounts/$account/withdrawals?status=$s&limit=1")" ] ||
      too_little "a withdrawal of $account is still $s: wait until it has finished"
  done
  id=$(first_id "/accounts/$account/deposits?status=credited&limit=1")
  [ -n "$credited_deposit" ] || [ -z "$id" ] || credited_deposit="$account $id"
  id=$(first_id "/accounts/$account/deposits?status=returned&limit=1")
  [ -n "$returned_deposit" ] || [ -z "$id" ] || returned_deposit="$account $id"
  id=$(first_id "/accounts/$account/withdrawals?status=executed&limit=1")
  [ -n "$executed" ] || [ -z "$id" ] || executed="$account $id"
  id=$(first_id "/accounts/$account/withdrawals?status=failed&limit=1")
  [ -n "$failed" ] || [ -z "$id" ] || failed="$account $id"
  # More than one page: the first page names a next one.
  case "$(field "$(curl -s "$API/accounts/$account/deposits?limit=$PAGE")" next_before)" in
    dep_*) multi_page=$((multi_page + 1)) ;;
  esac
done
[ -n "$credited_deposit" ] || too_little "no credited deposit"
[ -n "$returned_deposit" ] || too_little "no returned deposit"
[ -n "$executed" ] || too_little "no executed withdrawal"
[ -n "$failed" ] || too_little "no failed withdrawal"
[ "$multi_page" -gt 0 ] || too_little "no account has more than $PAGE deposits, so no list takes several pages"
# alice's newest deposit: its ID, for the cursors' edge cases, and its network_transaction_id,
# for the filter.
newest=$(curl -s "$API/accounts/$ALICE/deposits?limit=1")
first_deposit=$(field "$newest" id)
first_ntx=$(field "$newest" network_transaction_id)
[ -n "$first_deposit" ] || too_little "alice has no deposits"
echo "   deposits: credited ${credited_deposit#* }, returned ${returned_deposit#* };" \
  "withdrawals: executed ${executed#* }, failed ${failed#* }"

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

say "Accounts"
for account in $ACCOUNTS; do
  compare GET "/accounts/$account"
  expect_status 200
done
compare HEAD "/accounts/$ALICE"
compare GET "/accounts/$(unknown_id "$ALICE")"
for id in nope acct_123 "acct_${ALICE#acct_}x" "$(printf '%s' "$ALICE" | tr 'a-z' 'A-Z')" \
  "acct_8${ALICE#acct_?}" "pay_${ALICE#acct_}" "dep_${ALICE#acct_}" "wd_${ALICE#acct_}" \
  "${ALICE#acct_}" "acct_" "acct%5F${ALICE#acct_}"; do
  compare GET "/accounts/$id"
done

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

say "Deposits: every page, each cursor used on both implementations"
# walk <list path> <query> <first side: a|b>: every page, taking the next page's cursor
# alternately from one implementation's answer and the other's (both receive every page's
# request).
walk() {
  local before="" side=$3 pages=0
  while [ "$pages" -lt 500 ]; do
    compare GET "$1?$2${before:+&before=$before}"
    expect_status 200
    pages=$((pages + 1))
    before=$(field "$(cat "$work/$side.body")" next_before)
    [ -z "$before" ] || [ "$before" = null ] && return 0
    case "$side" in a) side=b ;; b) side=a ;; esac
  done
}
for account in $ACCOUNTS; do
  for first in a b; do
    walk "/accounts/$account/deposits" "limit=$PAGE" "$first"
    walk "/accounts/$account/deposits" "limit=$PAGE&include=history" "$first"
    # Filtered: the cursors are the filtered pages' own (slice 4 decision 70).
    walk "/accounts/$account/deposits" "limit=3&status=credited" "$first"
    walk "/accounts/$account/deposits" "limit=3&status=returned" "$first"
  done
  walk "/accounts/$account/deposits" "" a
done

say "Deposits: query parameters"
T="/accounts/$ALICE/deposits"
for q in limit=0 limit=1 limit=200 limit=201 limit=abc limit=-1 limit=1.5 limit= limit=01 \
  "limit=1&limit=2" limit=%31 limit=99999999999999999999 \
  before=nope "before=wd_${first_deposit#dep_}" "before=dep_8${first_deposit#dep_?}" \
  "before=$(unknown_id "$first_deposit")" "before=$first_deposit" before= \
  "before=$first_deposit&before=$first_deposit" \
  include=history include=nope include= "include=history&include=history" foo=bar \
  "limit=1&foo=bar"; do
  compare GET "$T?$q"
done
compare HEAD "$T?limit=1"

say "Deposits: filters"
# Every status an account's deposit can have (the demo's data has credited and returned; the rest
# are valid and absent), then the invalid ones: unmatched, unknown, empty, repeated.
for status in created received under_review approved rejected credited returned; do
  compare GET "$T?status=$status"
  expect_status 200
done
for q in status=unmatched status=nope status= status=CREDITED "status=credited&status=returned"; do
  compare GET "$T?$q"
done
# network_transaction_id: matching, a valid one that matches nothing, the same with a status that
# does and one that doesn't, and the invalid ones (empty, 256 characters, repeated).
compare GET "$T?network_transaction_id=$first_ntx"
expect_status 200
compare GET "$T?network_transaction_id=no-such-reference"
expect_status 200
for status in credited returned; do
  compare GET "$T?network_transaction_id=$first_ntx&status=$status"
done
compare GET "$T?network_transaction_id="
compare GET "$T?network_transaction_id=$(printf 'x%.0s' $(seq 1 256))"
compare GET "$T?network_transaction_id=a&network_transaction_id=b"
compare GET "/accounts/nope/deposits?network_transaction_id=&status=nope&include=x&before=nope&limit=0"
compare GET "/accounts/$(unknown_id "$ALICE")/deposits"
compare GET "/accounts/nope/deposits"
compare GET "/accounts/dep_${ALICE#acct_}/deposits"

say "Single deposits"
for pair in "$credited_deposit" "$returned_deposit"; do
  read -r account id <<< "$pair"
  compare GET "/accounts/$account/deposits/$id"
  expect_status 200
  compare GET "/accounts/$account/deposits/$id?include=history"
  expect_status 200
  for q in include=nope include= "include=history&include=history" foo=bar limit=0 before=nope; do
    compare GET "/accounts/$account/deposits/$id?$q"
  done
done
read -r account id <<< "$credited_deposit"
compare HEAD "/accounts/$account/deposits/$id"
# Another account's deposit, a real deposit under an unknown account, an unknown deposit.
for other in $ACCOUNTS; do
  [ "$other" = "$account" ] || compare GET "/accounts/$other/deposits/$id"
done
compare GET "/accounts/$(unknown_id "$account")/deposits/$id"
compare GET "/accounts/$account/deposits/$(unknown_id "$id")"
for bad in nope dep_123 "dep_${id#dep_}x" "$(printf '%s' "$id" | tr 'a-z' 'A-Z')" "dep_8${id#dep_?}" \
  "wd_${id#dep_}" "acct_${id#dep_}" "${id#dep_}" "dep_" "dep%5F${id#dep_}"; do
  compare GET "/accounts/$account/deposits/$bad"
done
compare GET "/accounts/nope/deposits/nope?include=nope"

say "Withdrawals: every page, each cursor used on both implementations"
for account in $ACCOUNTS; do
  for first in a b; do
    walk "/accounts/$account/withdrawals" "limit=2" "$first"
    walk "/accounts/$account/withdrawals" "limit=2&include=history" "$first"
    walk "/accounts/$account/withdrawals" "limit=2&status=executed" "$first"
  done
  walk "/accounts/$account/withdrawals" "" a
done

say "Withdrawals: query parameters and the status filter"
read -r account id <<< "$executed"
W="/accounts/$account/withdrawals"
for status in held submitted executed failed; do
  compare GET "$W?status=$status"
  expect_status 200
done
for q in status=nope status= status=HELD "status=held&status=failed" status=unmatched limit=0 \
  limit=201 limit=abc before=nope "before=dep_${id#wd_}" "before=$(unknown_id "$id")" "before=$id" \
  "before=$id&before=$id" \
  include=history include=nope "include=history&include=history" foo=bar; do
  compare GET "$W?$q"
done
compare HEAD "$W?limit=1"
compare GET "/accounts/$(unknown_id "$account")/withdrawals"
compare GET "/accounts/nope/withdrawals?status=nope&include=x&before=nope&limit=0"

say "Single withdrawals"
for pair in "$executed" "$failed"; do
  read -r account id <<< "$pair"
  compare GET "/accounts/$account/withdrawals/$id"
  expect_status 200
  compare GET "/accounts/$account/withdrawals/$id?include=history"
  expect_status 200
  for q in include=nope foo=bar limit=0 status=nope; do
    compare GET "/accounts/$account/withdrawals/$id?$q"
  done
done
read -r account id <<< "$executed"
compare HEAD "/accounts/$account/withdrawals/$id"
# Another account's withdrawal, a real withdrawal under an unknown account, an unknown withdrawal.
for other in $ACCOUNTS; do
  [ "$other" = "$account" ] || compare GET "/accounts/$other/withdrawals/$id"
done
compare GET "/accounts/$(unknown_id "$account")/withdrawals/$id"
compare GET "/accounts/$account/withdrawals/$(unknown_id "$id")"
for bad in nope wd_123 "dep_${id#wd_}" "acct_${id#wd_}" "wd_8${id#wd_?}" \
  "$(printf '%s' "$id" | tr 'a-z' 'A-Z')" "${id#wd_}"; do
  compare GET "/accounts/$account/withdrawals/$bad"
done
compare GET "/accounts/nope/withdrawals/nope?include=nope"

say "Transactions: every page, each cursor used on both implementations"
# Deposits and withdrawals together (slice 4 decision 67): carol's list has both, so a small page
# gives cursors of both kinds, each taken alternately from one implementation's answer.
for account in $ACCOUNTS; do
  for first in a b; do
    walk "/accounts/$account/transactions" "limit=3" "$first"
  done
  walk "/accounts/$account/transactions" "" a
done

say "Transactions: query parameters"
read -r account withdrawal <<< "$executed"
T="/accounts/$account/transactions"
# before of each kind, of another account (a position: decision 66), naming nothing, and the
# invalid ones; limit's edges; include and the filters, which this list ignores.
for q in "before=$withdrawal" "before=$first_deposit" "before=$(unknown_id "$withdrawal")" \
  "before=$(unknown_id "$first_deposit")" before=nope before= "before=dep_x" "before=acct_${withdrawal#wd_}" \
  "before=$(printf '%s' "$withdrawal" | tr 'a-z' 'A-Z')" "before=$withdrawal&before=$first_deposit" \
  limit=0 limit=1 limit=200 limit=201 limit=abc "limit=1&limit=2" \
  include=history status=held "network_transaction_id=x&include=history"; do
  compare GET "$T?$q"
done
compare HEAD "$T?limit=1"
compare GET "/accounts/$(unknown_id "$account")/transactions"
compare GET "/accounts/nope/transactions?before=nope&limit=0"

say "Unknown routes and wrong methods"
for path in / /nope /accounts "/accounts/$ALICE/" "/accounts/$ALICE/balance/" \
  "/accounts/$ALICE/nope" /withdrawals /withdrawals/ "/withdrawals/${executed#* }" \
  "/accounts/${executed% *}/withdrawals/${executed#* }/" "/accounts/$ALICE/withdrawals/" \
  "/Accounts/$ALICE/balance" /accounts//balance "//accounts/$ALICE/balance" \
  "/accounts/$ALICE/../$BOB/balance" /provider /provider/nope; do
  compare GET "$path"
done
for m in POST PUT PATCH DELETE OPTIONS; do
  compare "$m" "/accounts/$ALICE/balance"
done
compare POST "/accounts/$ALICE/deposits"
compare POST "/accounts/$ALICE/transactions"
compare DELETE "/accounts/${executed% *}/withdrawals/${executed#* }"
compare PUT "/accounts/${executed% *}/withdrawals/${executed#* }"
compare PUT "/accounts/$ALICE/withdrawals"
compare GET /provider/incoming-payments
compare GET /provider/transfer-updates
compare DELETE /provider/transfer-updates

say "Replays of the demo's withdrawal requests"
# The request rebuilt from the withdrawal's own fields (the response holds every field of the
# request): the same key and body returns the stored withdrawal (200), the same key with another
# amount the reuse error (409). Neither stores anything.
for pair in "$executed" "$failed"; do
  read -r account id <<< "$pair"
  w=$(curl -s "$API/accounts/$account/withdrawals/$id")
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
