#!/usr/bin/env bash -x
set -euo pipefail

# ---- config (override via env) ----
NS="${NS:-demo}"
FI_NAME="${FI_NAME:-fi-inbound-outbound-latency-timeout}"

# Cancellation trigger (your API supports spec.cancel=true)
CANCEL_PATCH="${CANCEL_PATCH:-{\"spec\":{\"cancel\":true}}}"

# Prefix for injected http route names inside VirtualService httpbin1 (inbound patch)
FI_RULE_PREFIX="${FI_RULE_PREFIX:-fi-${FI_NAME}-}"

# Prefix for managed VirtualServices created for OUTBOUND rules
#
# Managed outbound VirtualService names:
#   fi-<fi-name>-<action>
#
# Injected HTTP rule names (inside VS.spec.http[].name):
#   fi-<fi-name>-<direction>-<action>
#
MANAGED_VS_PREFIX="${MANAGED_VS_PREFIX:-fi-${FI_NAME}-}"

# Timeouts
WAIT_PHASE_TIMEOUT="${WAIT_PHASE_TIMEOUT:-180}"
WAIT_CLEANUP_TIMEOUT="${WAIT_CLEANUP_TIMEOUT:-180}"

ok()   { printf "✅ %s\n" "$1"; }
fail() { printf "❌ %s\n" "$1"; exit 1; }
info() { printf "ℹ️  %s\n" "$1"; }

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

# Run from curl-client pod
in_pod() {
  kubectl -n "$NS" exec curl-client -- sh -lc "$*"
}

cancel_fi() {
  info "Triggering cancellation: patch FaultInjection/$FI_NAME with: $CANCEL_PATCH"
  kubectl -n "$NS" patch faultinjection "$FI_NAME" --type=merge -p "$CANCEL_PATCH" >/dev/null \
    && ok "Cancellation patch applied" \
    || fail "Failed to patch FaultInjection for cancellation"
}

wait_phase_cancelled_or_completed() {
  timeout_s="${1:-180}"
  step_s="${2:-2}"
  i=0

  info "Waiting for FaultInjection/$FI_NAME status.phase=Cancelled|Completed (timeout=${timeout_s}s)..."
  while [ "$i" -lt "$timeout_s" ]; do
    phase="$(kubectl -n "$NS" get faultinjection "$FI_NAME" -o jsonpath="{.status.phase}" 2>/dev/null || true)"
    msg="$(kubectl -n "$NS" get faultinjection "$FI_NAME" -o jsonpath="{.status.message}" 2>/dev/null || true)"
    [ -n "$phase" ] && printf "  phase=%s msg=%s\n" "$phase" "$msg"

    if [ "$phase" = "Cancelled" ] || [ "$phase" = "Completed" ]; then
      ok "FaultInjection phase is $phase"
      return 0
    fi

    sleep "$step_s"
    i=$((i+step_s))
  done

  fail "Timed out waiting for FaultInjection to reach Cancelled/Completed"
}

wait_cleanup_gone() {
  timeout_s="${1:-180}"
  step_s="${2:-2}"
  i=0

  info "Waiting for cleanup: no managed VirtualServices (${MANAGED_VS_PREFIX}*) and no injected rules in httpbin1 (prefix=${FI_RULE_PREFIX}) (timeout=${timeout_s}s)..."
  while [ "$i" -lt "$timeout_s" ]; do
    # 1) OUTBOUND: managed VS should be deleted
    managed_vs="$(kubectl -n "$NS" get virtualservice -o name 2>/dev/null | sed 's|^virtualservice.networking.istio.io/||' | grep -E "^${MANAGED_VS_PREFIX}" || true)"

    # 2) Injected rules should be removed from ALL VirtualServices
    all_rule_names="$(kubectl -n "$NS" get virtualservice -o jsonpath='{range .items[*]}{range .spec.http[*]}{.name}{"\n"}{end}{end}' 2>/dev/null || true)"
    injected_anywhere=""
    if [ -n "$all_rule_names" ]; then
      injected_anywhere="$(printf "%s\n" "$all_rule_names" | grep -E "^${FI_RULE_PREFIX}" || true)"
    fi

    if [ -z "$managed_vs" ] && [ -z "$injected_anywhere" ]; then
      ok "Cleanup confirmed (no managed VS + no injected rules anywhere)"
      return 0
    fi


    sleep "$step_s"
    i=$((i+step_s))
  done

  printf "---- managed VS still present ----\n%s\n" "${managed_vs:-<none>}"
  printf "---- injected rules still present (any VS) ----\n%s\n" "${injected_anywhere:-<none>}"
  fail "Timed out waiting for cleanup to finish"
}


echo "=== PRE-CANCEL (assert faults active) ==="

# OUTBOUND active (curl-client -> httpbin2)
o1="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/test")"
printf "%s\n" "$o1"
assert_code "OUTBOUND CTRL" "200" "$o1"
assert_lt   "OUTBOUND CTRL fast" "0.5" "$o1"

o2="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin2:8000/anything/test")"
printf "%s\n" "$o2"
assert_code "OUTBOUND ABORT" "504" "$o2"
assert_lt   "OUTBOUND ABORT fast" "0.5" "$o2"

o3="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/vendors/test")"
printf "%s\n" "$o3"
assert_code "OUTBOUND DELAY" "200" "$o3"
assert_ge   "OUTBOUND DELAY >=2s" "1.8" "$o3"

o4="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND NOHIT code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/other/test")"
printf "%s\n" "$o4"
assert_code "OUTBOUND NOHIT" "200" "$o4"
assert_lt   "OUTBOUND NOHIT fast" "0.5" "$o4"

# INBOUND active (httpbin1)
i1="$(in_pod "curl -s -o /dev/null -w \"INBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/test")"
printf "%s\n" "$i1"
assert_code "INBOUND CTRL" "200" "$i1"
assert_lt   "INBOUND CTRL fast" "0.5" "$i1"

i2="$(in_pod "curl -s -o /dev/null -w \"INBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/vendors/test")"
printf "%s\n" "$i2"
assert_code "INBOUND DELAY" "200" "$i2"
assert_ge   "INBOUND DELAY >=2s" "1.8" "$i2"

i3="$(in_pod "curl -s -o /dev/null -w \"INBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin1:8000/anything/test")"
printf "%s\n" "$i3"
assert_code "INBOUND ABORT" "504" "$i3"
assert_lt   "INBOUND ABORT fast" "0.5" "$i3"

echo "=== CANCEL ==="
cancel_fi

wait_phase_cancelled_or_completed "$WAIT_PHASE_TIMEOUT" 2
wait_cleanup_gone "$WAIT_CLEANUP_TIMEOUT" 2

echo "=== POST-CANCEL (assert faults removed) ==="

# OUTBOUND should be normal now
po1="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/test")"
printf "%s\n" "$po1"
assert_code "OUTBOUND CTRL" "200" "$po1"
assert_lt   "OUTBOUND CTRL fast" "0.5" "$po1"

po2="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND ABORT baseline code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin2:8000/anything/test")"
printf "%s\n" "$po2"
assert_code "OUTBOUND ABORT baseline" "504" "$po2"
assert_lt   "OUTBOUND ABORT baseline fast" "0.5" "$po2"

po3="$(in_pod "curl -s -o /dev/null -w \"OUTBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/vendors/test")"
printf "%s\n" "$po3"
assert_code "OUTBOUND DELAY baseline" "200" "$po3"
assert_ge   "OUTBOUND DELAY baseline >=2s" "1.8" "$po3"

# INBOUND should be normal now
pi1="$(in_pod "curl -s -o /dev/null -w \"INBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/test")"
printf "%s\n" "$pi1"
assert_code "INBOUND CTRL" "200" "$pi1"
assert_lt   "INBOUND CTRL fast" "0.5" "$pi1"

pi2="$(in_pod "curl -s -o /dev/null -w \"INBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/vendors/test")"
printf "%s\n" "$pi2"
assert_code "INBOUND DELAY should stop" "200" "$pi2"
assert_lt   "INBOUND DELAY should stop fast" "0.5" "$pi2"

pi3="$(in_pod "curl -s -o /dev/null -w \"INBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin1:8000/anything/test")"
printf "%s\n" "$pi3"
assert_code "INBOUND ABORT should stop" "200" "$pi3"
assert_lt   "INBOUND ABORT should stop fast" "0.5" "$pi3"

echo "=== ENVOY STATS (smoke) ==="
in_pod "curl -s localhost:15000/stats | grep -n \"httpbin2.demo.svc.cluster.local\" | head -n 5 || true"

echo "✅ ALL CANCELLATION TESTS PASSED"
