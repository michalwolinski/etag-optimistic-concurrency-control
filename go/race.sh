#!/usr/bin/env bash
#
# Two clients, one resource, one validator. Whatever the scheduler does, exactly
# one of them is allowed to move the resource - and the wire says which.
#
# Every line below is exactly one request.
#
# Usage: ./race.sh [port]

set -euo pipefail

port="${1:-18080}"
base="http://127.0.0.1:${port}"
tmp="$(mktemp -d)"

go build -o "${tmp}/etagdemo" .
"${tmp}/etagdemo" ":${port}" >"${tmp}/server.log" 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true; wait "$server" 2>/dev/null || true; rm -rf "$tmp"' EXIT

for _ in $(seq 1 150); do
	curl -fsS "${base}/orders/1" >/dev/null 2>&1 && break
done

# req <tmpfile> [curl args...] -> prints "<status> <etag|-> <body>"
req() {
	local out="$1"
	shift
	local code etag
	code=$(curl -sS -o "${out}.body" -D "${out}.headers" -w '%{http_code}' "$@")
	etag=$(tr -d '\r' <"${out}.headers" | awk 'tolower($1) == "etag:" {print $2}')
	printf '%s  ETag: %-16s %s' "$code" "${etag:--}" "$(cat "${out}.body" 2>/dev/null || true)"
}

# conditional_put <tmpfile> <if-match> <status>
conditional_put() {
	local out="$1" if_match="$2" status="$3"
	req "$out" -X PUT \
		-H "If-Match: ${if_match}" \
		-H 'Content-Type: application/json' \
		-d "{\"status\":\"${status}\"}" \
		"${base}/orders/1"
}

echo "### 1. Read the resource once - this validator is what both clients will hold"
printf 'GET  /orders/1                                  -> %s\n' "$(req "${tmp}/g1" "${base}/orders/1")"

echo
echo "### 2. The same header, the other half of its job: cache validation"
printf 'GET  /orders/1  If-None-Match: "order-1-v1"     -> %s\n' \
	"$(req "${tmp}/g2" -H 'If-None-Match: "order-1-v1"' "${base}/orders/1")"
printf 'GET  /orders/1  If-None-Match: W/"order-1-v1"   -> %s\n' \
	"$(req "${tmp}/g3" -H 'If-None-Match: W/"order-1-v1"' "${base}/orders/1")"

echo
echo "### 3. A write that carries no precondition at all"
printf 'PUT  /orders/1  (no If-Match)                   -> %s\n' \
	"$(req "${tmp}/p1" -X PUT -H 'Content-Type: application/json' -d '{"status":"paid"}' "${base}/orders/1")"

echo
echo "### 4. The race: both clients released together, both holding the same validator"
conditional_put "${tmp}/w1" '"order-1-v1"' paid >"${tmp}/r1" &
p1=$!
conditional_put "${tmp}/w2" '"order-1-v1"' shipped >"${tmp}/r2" &
p2=$!
wait "$p1" "$p2"
printf 'PUT  /orders/1  If-Match: "order-1-v1"  paid    -> %s\n' "$(cat "${tmp}/r1")"
printf 'PUT  /orders/1  If-Match: "order-1-v1"  shipped -> %s\n' "$(cat "${tmp}/r2")"

echo
echo "### 5. The resource afterwards: written once, version advanced once"
printf 'GET  /orders/1                                  -> %s\n' "$(req "${tmp}/g4" "${base}/orders/1")"

echo
echo "### 6. The rejected client retries with the validator it was handed"
printf 'PUT  /orders/1  If-Match: "order-1-v2"  shipped -> %s\n' \
	"$(conditional_put "${tmp}/w3" '"order-1-v2"' shipped)"
