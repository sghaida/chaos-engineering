kubectl -n demo exec curl-client -- sh -lc '
set -eu

ok()   { printf "✅ %s\n" "$1"; }
fail() { printf "❌ %s\n" "$1"; exit 1; }

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
  awk -v t="$total" -v m="$min" "BEGIN{ exit !(t>=m) }" && ok "$name (total=$total>=${min})" || fail "$name expected total>=${min} got total=${total:-?}"
}

assert_lt() {
  name="$1"; max="$2"; out="$3"
  total="$(printf "%s" "$out" | sed -n "s/.*total=\\([0-9.]*\\).*/\\1/p" | tail -n1)"
  awk -v t="$total" -v m="$max" "BEGIN{ exit !(t<m) }" && ok "$name (total=$total<${max})" || fail "$name expected total<${max} got total=${total:-?}"
}

echo "=== OUTBOUND ==="

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

echo "=== INBOUND ==="

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

echo "=== ENVOY STATS (smoke) ==="
curl -s localhost:15000/stats | grep -F "httpbin2.demo.svc.cluster.local" | head -n 5 || true

echo "✅ ALL TESTS PASSED"
'