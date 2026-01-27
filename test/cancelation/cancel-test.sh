kubectl -n demo exec curl-client -- sh -lc '
set -eu

ok()   { printf "✅ %s\n" "$1"; }
fail() { printf "❌ %s\n" "$1"; exit 1; }
info() { printf "ℹ️  %s\n" "$1"; }

run() {
  name="$1"; shift
  out="$(sh -lc "$*" 2>/dev/null || true)"
  printf "%s\n" "$out"
}

assert_code() {
  name="$1"; want="$2"; out="$3"
  code="$(printf "%s" "$out" | sed -n "s/.*code=\\([0-9][0-9][0-9]\\).*/\\1/p" | tail -n1)"
  [ "$code" = "$want" ] && ok "$name (code=$code)" || fail "$name expected code=$want got code=${code:-?}"
}

assert_ge() {
  name="$1"; min="$2"; out="$3"
  total="$(printf "%s" "$out" | sed -n "s/.*total=\\([0-9.]*\\).*/\\1/p" | tail -n1)"
  awk -v t="$total" -v m="$min" "BEGIN{ exit !(t>=m) }" \
    && ok "$name (total=$total>=${min})" \
    || fail "$name expected total>=${min} got total=${total:-?}"
}

assert_lt() {
  name="$1"; max="$2"; out="$3"
  total="$(printf "%s" "$out" | sed -n "s/.*total=\\([0-9.]*\\).*/\\1/p" | tail -n1)"
  awk -v t="$total" -v m="$max" "BEGIN{ exit !(t<m) }" \
    && ok "$name (total=$total<${max})" \
    || fail "$name expected total<${max} got total=${total:-?}"
}

# Wait until ALL injected rules disappear from the VirtualServices.
# Assumes your controller uses a deterministic prefix in rule names.
# Update FI_RULE_PREFIX if your prefix differs.
FI_RULE_PREFIX="${FI_RULE_PREFIX:-fi-}"
NAMESPACE="${NAMESPACE:-demo}"

wait_no_fi_rules() {
  timeout_s="${1:-120}"
  step_s="${2:-2}"

  info "Waiting for injected rules to be removed (prefix=${FI_RULE_PREFIX}, timeout=${timeout_s}s)..."
  i=0
  while [ "$i" -lt "$timeout_s" ]; do
    h1="$(kubectl -n "$NAMESPACE" get vs httpbin1 -o jsonpath="{range .spec.http[*]}{.name}{\"\\n\"}{end}" 2>/dev/null || true)"
    h2="$(kubectl -n "$NAMESPACE" get vs httpbin2 -o jsonpath="{range .spec.http[*]}{.name}{\"\\n\"}{end}" 2>/dev/null || true)"

    if printf "%s\n%s\n" "$h1" "$h2" | grep -q "^${FI_RULE_PREFIX}"; then
      sleep "$step_s"
      i=$((i+step_s))
      continue
    fi

    ok "No injected rules found in VS httpbin1/httpbin2"
    return 0
  done

  printf "---- httpbin1 http names ----\n%s\n" "$h1"
  printf "---- httpbin2 http names ----\n%s\n" "$h2"
  fail "Timed out waiting for injected rules to be removed"
}

# Trigger cancellation:
# By default this PATCH adds spec.cancelled=true on the FaultInjection CR.
# If your controller uses a different field/path, override CANCEL_PATCH.
FI_NAME="${FI_NAME:-fi-inbound-outbound-latency-timeout}"
CANCEL_PATCH="${CANCEL_PATCH:-{\"spec\":{\"cancelled\":true}}}"

cancel_fi() {
  info "Triggering cancellation for FaultInjection/${FI_NAME} ..."
  kubectl -n "$NAMESPACE" patch faultinjection "$FI_NAME" --type=merge -p "$CANCEL_PATCH" >/dev/null \
    && ok "Cancellation patch applied" \
    || fail "Failed to patch FaultInjection for cancellation"
}

wait_phase_completed() {
  timeout_s="${1:-120}"
  step_s="${2:-2}"

  info "Waiting for FaultInjection/${FI_NAME} status.phase=Completed (timeout=${timeout_s}s)..."
  i=0
  while [ "$i" -lt "$timeout_s" ]; do
    phase="$(kubectl -n "$NAMESPACE" get faultinjection "$FI_NAME" -o jsonpath="{.status.phase}" 2>/dev/null || true)"
    msg="$(kubectl -n "$NAMESPACE" get faultinjection "$FI_NAME" -o jsonpath="{.status.message}" 2>/dev/null || true)"
    [ -n "$phase" ] && printf "phase=%s msg=%s\n" "$phase" "$msg" | sed "s/^/  /"

    if [ "$phase" = "Completed" ]; then
      ok "FaultInjection phase is Completed"
      return 0
    fi

    sleep "$step_s"
    i=$((i+step_s))
  done

  fail "Timed out waiting for FaultInjection to reach Completed"
}

echo "=== PRE-CANCEL (ensure faults are active) ==="

echo "=== OUTBOUND (active) ==="
o1=$(run "OUTBOUND CTRL"  "curl -s -o /dev/null -w \"OUTBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/test")
printf "%s\n" "$o1"
assert_code "OUTBOUND CTRL"  "200" "$o1"
assert_lt   "OUTBOUND CTRL fast" "0.5" "$o1"

o2=$(run "OUTBOUND ABORT" "curl -s -o /dev/null -w \"OUTBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin2:8000/anything/test")
printf "%s\n" "$o2"
assert_code "OUTBOUND ABORT" "504" "$o2"
assert_lt   "OUTBOUND ABORT fast" "0.5" "$o2"

o3=$(run "OUTBOUND DELAY" "curl -s -o /dev/null -w \"OUTBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/vendors/test")
printf "%s\n" "$o3"
assert_code "OUTBOUND DELAY" "200" "$o3"
assert_ge   "OUTBOUND DELAY >=2s" "1.8" "$o3"

o4=$(run "OUTBOUND NOHIT" "curl -s -o /dev/null -w \"OUTBOUND NOHIT code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/other/test")
printf "%s\n" "$o4"
assert_code "OUTBOUND NOHIT" "200" "$o4"
assert_lt   "OUTBOUND NOHIT fast" "0.5" "$o4"

echo "=== INBOUND (active) ==="
i1=$(run "INBOUND CTRL"  "curl -s -o /dev/null -w \"INBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/test")
printf "%s\n" "$i1"
assert_code "INBOUND CTRL"  "200" "$i1"
assert_lt   "INBOUND CTRL fast" "0.5" "$i1"

i2=$(run "INBOUND DELAY" "curl -s -o /dev/null -w \"INBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/vendors/test")
printf "%s\n" "$i2"
assert_code "INBOUND DELAY" "200" "$i2"
assert_ge   "INBOUND DELAY >=2s" "1.8" "$i2"

i3=$(run "INBOUND ABORT" "curl -s -o /dev/null -w \"INBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin1:8000/anything/test")
printf "%s\n" "$i3"
assert_code "INBOUND ABORT" "504" "$i3"
assert_lt   "INBOUND ABORT fast" "0.5" "$i3"

echo "=== CANCEL ==="
cancel_fi

# Optional: ensure the controller actually updated status.phase etc.
wait_phase_completed 120 2

# Ensure injected rules are removed from both VirtualServices.
# Override FI_RULE_PREFIX if your injected rule name prefix differs.
wait_no_fi_rules 120 2

echo "=== POST-CANCEL (ensure faults are gone) ==="

echo "=== OUTBOUND (should be normal) ==="
po1=$(run "OUTBOUND CTRL"  "curl -s -o /dev/null -w \"OUTBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/test")
printf "%s\n" "$po1"
assert_code "OUTBOUND CTRL"  "200" "$po1"
assert_lt   "OUTBOUND CTRL fast" "0.5" "$po1"

po2=$(run "OUTBOUND ABORT should stop" "curl -s -o /dev/null -w \"OUTBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin2:8000/anything/test")
printf "%s\n" "$po2"
assert_code "OUTBOUND ABORT should stop" "200" "$po2"
assert_lt   "OUTBOUND ABORT should stop fast" "0.5" "$po2"

po3=$(run "OUTBOUND DELAY should stop" "curl -s -o /dev/null -w \"OUTBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin2:8000/anything/vendors/test")
printf "%s\n" "$po3"
assert_code "OUTBOUND DELAY should stop" "200" "$po3"
assert_lt   "OUTBOUND DELAY should stop fast" "0.5" "$po3"

echo "=== INBOUND (should be normal) ==="
pi1=$(run "INBOUND CTRL"  "curl -s -o /dev/null -w \"INBOUND CTRL  code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/test")
printf "%s\n" "$pi1"
assert_code "INBOUND CTRL"  "200" "$pi1"
assert_lt   "INBOUND CTRL fast" "0.5" "$pi1"

pi2=$(run "INBOUND DELAY should stop" "curl -s -o /dev/null -w \"INBOUND DELAY code=%{http_code} total=%{time_total}\\n\" http://httpbin1:8000/anything/vendors/test")
printf "%s\n" "$pi2"
assert_code "INBOUND DELAY should stop" "200" "$pi2"
assert_lt   "INBOUND DELAY should stop fast" "0.5" "$pi2"

pi3=$(run "INBOUND ABORT should stop" "curl -s -o /dev/null -w \"INBOUND ABORT code=%{http_code} total=%{time_total}\\n\" -H \"x-chaos-mode: timeout\" http://httpbin1:8000/anything/test")
printf "%s\n" "$pi3"
assert_code "INBOUND ABORT should stop" "200" "$pi3"
assert_lt   "INBOUND ABORT should stop fast" "0.5" "$pi3"

echo "=== ENVOY STATS (smoke) ==="
curl -s localhost:15000/stats | grep -F "httpbin2.demo.svc.cluster.local" | head -n 5 || true

echo "✅ ALL CANCELATION TESTS PASSED"
'
