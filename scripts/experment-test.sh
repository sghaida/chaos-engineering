#!/usr/bin/env bash
set -euo pipefail

# ---- config (override via env) ----
NS="${NS:-demo}"
CURL_POD_LABEL_SELECTOR="${CURL_POD_LABEL_SELECTOR:-app=curl-client}"

# Targets
HTTPBIN1_URL="${HTTPBIN1_URL:-http://httpbin1:8000}"
HTTPBIN2_URL="${HTTPBIN2_URL:-http://httpbin2:8000}"

# Thresholds
FAST_MAX_S="${FAST_MAX_S:-0.5}"
DELAY_MIN_S="${DELAY_MIN_S:-1.8}"

ok()   { printf "✅ %s\n" "$1"; }
fail() { printf "❌ %s\n" "$1"; exit 1; }
info() { printf "ℹ️  %s\n" "$1"; }

# Run a command inside the curl-client pod
in_pod() {
  # Prefer label selector (works whether it’s a Deployment/Pod)
  pod="$(kubectl -n "$NS" get pod -l "${CURL_POD_LABEL_SELECTOR:-app=curl-client}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"

  # Fallback to known pod name used in your env (curl-client)
  if [ -z "${pod:-}" ]; then
    pod="curl-client"
  fi

  kubectl -n "$NS" exec "$pod" -- sh -lc "$*"
}

assert_code() {
  name="$1"; want="$2"; out="$3"
  code="$(printf "%s" "$out" | sed -n "s/.*code=\\([0-9][0-9][0-9]\\).*/\\1/p" | tail -n1)"
  [ "$code" = "$want" ] && ok "$name (code=$code)" || fail "$name expected code=$want got code=${code:-?}"
}

assert_ge() {
  name="$1"; min="$2"; out="$3"
  total="$(printf "%s" "$out" | sed -n "s/.*total=\\([0-9.]*\\).*/\\1/p" | tail -n1)"
  awk -v t="$total" -v m="$min" 'BEGIN{ exit !(t>=m) }' \
    && ok "$name (total=$total>=${min})" \
    || fail "$name expected total>=${min} got total=${total:-?}"
}

assert_lt() {
  name="$1"; max="$2"; out="$3"
  total="$(printf "%s" "$out" | sed -n "s/.*total=\\([0-9.]*\\).*/\\1/p" | tail -n1)"
  awk -v t="$total" -v m="$max" 'BEGIN{ exit !(t<m) }' \
    && ok "$name (total=$total<${max})" \
    || fail "$name expected total<${max} got total=${total:-?}"
}

echo "=== PRECHECK: envoy admin ready (inside curl-client) ==="
s1="$(in_pod "curl -s -o /dev/null -w \"ENVOY ADMIN code=%{http_code} total=%{time_total}\\n\" localhost:15000/stats")"
printf "%s\n" "$s1"
assert_code "ENVOY ADMIN" "200" "$s1"

echo "=== OUTBOUND (curl-client -> httpbin2) ==="

o1="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" ${HTTPBIN2_URL}/anything/test")"
printf "%s\n" "$o1"
assert_code "OUTBOUND CTRL" "200" "$o1"
assert_lt   "OUTBOUND CTRL fast" "$FAST_MAX_S" "$o1"

o2="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" ${HTTPBIN2_URL}/anything/test")"
printf "%s\n" "$o2"
assert_code "OUTBOUND ABORT" "504" "$o2"
assert_lt   "OUTBOUND ABORT fast" "$FAST_MAX_S" "$o2"

o3="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND DELAY code=%{http_code} total=%{time_total}\\n\" ${HTTPBIN2_URL}/anything/vendors/test")"
printf "%s\n" "$o3"
assert_code "OUTBOUND DELAY" "200" "$o3"
assert_ge   "OUTBOUND DELAY >=2s" "$DELAY_MIN_S" "$o3"

o4="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND NOHIT code=%{http_code} total=%{time_total}\\n\" ${HTTPBIN2_URL}/anything/other/test")"
printf "%s\n" "$o4"
assert_code "OUTBOUND NOHIT" "200" "$o4"
assert_lt   "OUTBOUND NOHIT fast" "$FAST_MAX_S" "$o4"

echo "=== INBOUND (httpbin1) ==="

i1="$(in_pod "curl -s -o /dev/null -w \"INBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" ${HTTPBIN1_URL}/anything/test")"
printf "%s\n" "$i1"
assert_code "INBOUND CTRL" "200" "$i1"
assert_lt   "INBOUND CTRL fast" "$FAST_MAX_S" "$i1"

i2="$(in_pod "curl -s -o /dev/null -w \"INBOUND DELAY code=%{http_code} total=%{time_total}\\n\" ${HTTPBIN1_URL}/anything/vendors/test")"
printf "%s\n" "$i2"
assert_code "INBOUND DELAY" "200" "$i2"
assert_ge   "INBOUND DELAY >=2s" "$DELAY_MIN_S" "$i2"

i3="$(in_pod "curl -s -o /dev/null -w \"INBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" ${HTTPBIN1_URL}/anything/test")"
printf "%s\n" "$i3"
assert_code "INBOUND ABORT" "504" "$i3"
assert_lt   "INBOUND ABORT fast" "$FAST_MAX_S" "$i3"

echo "=== ENVOY STATS (smoke) ==="
in_pod "curl -s localhost:15000/stats | grep -F \"httpbin2.demo.svc.cluster.local\" | head -n 5 || true"

echo "✅ ALL EXPERIMENT TESTS PASSED"
