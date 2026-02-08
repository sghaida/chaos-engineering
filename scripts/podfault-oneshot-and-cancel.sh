#!/usr/bin/env bash
set -euo pipefail

# -------------------
# Config
# -------------------
NS="${NS:-podfault-test}"
APP_LABEL="${APP_LABEL:-app=podfault-victim}"

FI_FORCEFUL="${FI_FORCEFUL:-fi-poddelete-forceful-oneshot}"
FI_GRACEFUL="${FI_GRACEFUL:-fi-poddelete-graceful-oneshot}"
FI_CANCEL="${FI_CANCEL:-fi-poddelete-cancel-after-forceful}"

WAIT_READY_TIMEOUT="${WAIT_READY_TIMEOUT:-180}"
WAIT_STATUS_TIMEOUT="${WAIT_STATUS_TIMEOUT:-240}"

# Use the exact files you requested
WORKLOADS_MANIFEST="${WORKLOADS_MANIFEST:-../resources/workloads-podfault.yaml}"
PODFAULTS_MANIFEST="${PODFAULTS_MANIFEST:-../resources/fi-scenarios/pod-fault.yaml}"

ok()   { printf "✅ %s\n" "$1"; }
fail() { printf "❌ %s\n" "$1"; exit 1; }
info() { printf "ℹ️  %s\n" "$1"; }

now_s() { date +%s; }

# -------------------
# Strict apply + debug
# -------------------
ensure_ns() {
  if ! kubectl get ns "$NS" >/dev/null 2>&1; then
    info "Creating namespace: $NS"
    kubectl create ns "$NS" >/dev/null
  fi
  ok "Namespace ready: $NS"
}

apply_file_strict() {
  local file="$1"
  [ -f "$file" ] || fail "Manifest not found: $file"

  info "Applying: $file"
  # Do NOT suppress output or errors; if this fails we want to see it.
  kubectl apply -f "$file"
  ok "Applied: $file (namespace=$NS)"
}

debug_dump_after_missing_fi() {
  local fi="$1"
  printf "\n==== DEBUG: Missing FaultInjection/%s in ns=%s ====\n" "$fi" "$NS"

  echo "-- current context --"
  kubectl config current-context || true

  echo "-- file existence & snippet --"
  ls -la "$PODFAULTS_MANIFEST" || true
  grep -nE '^(apiVersion:|kind: FaultInjection|metadata:|  name: fi-|  namespace: )' "$PODFAULTS_MANIFEST" || true

  echo "-- server-side dry-run validation (shows admission/schema errors) --"
  kubectl apply -f "$PODFAULTS_MANIFEST" --dry-run=server -o yaml || true

  echo "-- FaultInjections in ns --"
  kubectl -n "$NS" get faultinjection || true

  echo "-- recent events in ns --"
  kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -n 50 || true

  echo "-- controller pods --"
  kubectl -n fi-operator-system get pods -l control-plane=controller-manager -o wide || true

  echo "-- controller logs (last 200 lines) --"
  kubectl -n fi-operator-system logs -l control-plane=controller-manager --since=20m --tail=200 || true

  printf "==== END DEBUG ====\n\n"
}

assert_fi_exists_or_debug() {
  local fi="$1"
  if ! kubectl -n "$NS" get faultinjection "$fi" >/dev/null 2>&1; then
    debug_dump_after_missing_fi "$fi"
    fail "FaultInjection/$fi not found in ns=$NS (check $PODFAULTS_MANIFEST)"
  fi
  ok "Found FaultInjection/$fi in ns=$NS"
}

# -------------------
# K8s helpers
# -------------------
pods_ready() {
  kubectl -n "$NS" get pod -l "$APP_LABEL" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null \
    | grep -c '^true$' || true
}

wait_ready_replicas() {
  local want="${1:-3}"
  local timeout_s="${2:-180}"
  local step_s=2
  local i=0

  info "Waiting for ${want} ready pods (label=$APP_LABEL) ..."
  while [ "$i" -lt "$timeout_s" ]; do
    local r
    r="$(pods_ready)"
    printf "  ready=%s/%s\n" "$r" "$want"
    if [ "$r" -ge "$want" ]; then
      ok "Pods ready (ready=$r)"
      return 0
    fi
    sleep "$step_s"
    i=$((i+step_s))
  done

  kubectl -n "$NS" get pod -l "$APP_LABEL" -owide || true
  fail "Timed out waiting for ready pods"
}

pick_one_pod() {
  kubectl -n "$NS" get pod -l "$APP_LABEL" -o jsonpath='{.items[0].metadata.name}'
}

pod_exists() {
  local p="$1"
  kubectl -n "$NS" get pod "$p" >/dev/null 2>&1
}

wait_pod_gone() {
  local p="$1"
  local timeout_s="${2:-120}"
  local step_s=1
  local i=0

  info "Waiting for pod to be deleted: $p"
  while [ "$i" -lt "$timeout_s" ]; do
    if ! pod_exists "$p"; then
      ok "Pod deleted: $p"
      return 0
    fi
    sleep "$step_s"
    i=$((i+step_s))
  done
  fail "Timed out waiting for pod deletion: $p"
}

wait_podfault_action_succeeded() {
  local fi="$1"
  local action="$2"
  local timeout_s="${3:-240}"
  local step_s=2
  local i=0

  info "Waiting for status.podFaults entry to reach Succeeded (fi=$fi action=$action)..."
  while [ "$i" -lt "$timeout_s" ]; do
    local line
    line="$(kubectl -n "$NS" get faultinjection "$fi" \
      -o jsonpath="{range .status.podFaults[?(@.actionName=='$action')]}{.tickId} {.state} {.jobName}{'\n'}{end}" \
      2>/dev/null || true)"
    [ -n "$line" ] && printf "  %s\n" "$line"

    if printf "%s\n" "$line" | grep -q '^oneshot Succeeded '; then
      ok "PodFault action succeeded (fi=$fi action=$action)"
      return 0
    fi

    sleep "$step_s"
    i=$((i+step_s))
  done

  kubectl -n "$NS" get faultinjection "$fi" -oyaml | sed -n '/status:/,$p' || true
  fail "Timed out waiting for pod fault action to succeed (fi=$fi action=$action)"
}

cancel_fi() {
  local fi="$1"
  info "Patching spec.cancel=true for $fi"
  kubectl -n "$NS" patch faultinjection "$fi" --type=merge -p '{"spec":{"cancel":true}}'
  ok "Cancellation patch applied: $fi"
}

assert_no_action_tick_exists() {
  local fi="$1"
  local action="$2"
  local timeout_s="${3:-90}"
  local step_s=2
  local i=0

  info "Asserting NO podFault tick appears for action=$action (fi=$fi) for ${timeout_s}s..."
  while [ "$i" -lt "$timeout_s" ]; do
    local line
    line="$(kubectl -n "$NS" get faultinjection "$fi" \
      -o jsonpath="{range .status.podFaults[?(@.actionName=='$action')]}{.tickId} {.state}{'\n'}{end}" \
      2>/dev/null || true)"
    if [ -n "$line" ]; then
      printf "  unexpected tick(s):\n%s\n" "$line"
      fail "Found unexpected podFault status entries for action=$action"
    fi
    sleep "$step_s"
    i=$((i+step_s))
  done
  ok "No podFault tick observed for action=$action"
}

delete_fi_if_exists() {
  local fi="$1"
  kubectl -n "$NS" delete faultinjection "$fi" --ignore-not-found=true >/dev/null 2>&1 || true
}

# -------------------
# Main
# -------------------
echo "=== SETUP (namespace + workload + pod-fault scenarios) ==="
ensure_ns
apply_file_strict "$WORKLOADS_MANIFEST"
apply_file_strict "$PODFAULTS_MANIFEST"
wait_ready_replicas 3 "$WAIT_READY_TIMEOUT"

echo "=== TEST 1: FORCEFUL ONE_SHOT ==="
delete_fi_if_exists "$FI_FORCEFUL"
apply_file_strict "$PODFAULTS_MANIFEST"
assert_fi_exists_or_debug "$FI_FORCEFUL"

target="$(pick_one_pod)"
info "Target baseline pod: $target"

t0="$(now_s)"
wait_podfault_action_succeeded "$FI_FORCEFUL" "forceful-oneshot" "$WAIT_STATUS_TIMEOUT"
wait_pod_gone "$target" 60
wait_ready_replicas 3 "$WAIT_READY_TIMEOUT"
t1="$(now_s)"
dt=$((t1-t0))
info "Forceful deletion elapsed=${dt}s"
[ "$dt" -lt 10 ] && ok "Forceful deletion was fast (<10s)" || fail "Forceful deletion was too slow (elapsed=${dt}s) — expected <10s"

echo "=== TEST 2: GRACEFUL ONE_SHOT (should be slower due to preStop sleep) ==="
delete_fi_if_exists "$FI_GRACEFUL"
apply_file_strict "$PODFAULTS_MANIFEST"
assert_fi_exists_or_debug "$FI_GRACEFUL"

target="$(pick_one_pod)"
info "Target baseline pod: $target"

t0="$(now_s)"
wait_podfault_action_succeeded "$FI_GRACEFUL" "graceful-oneshot" "$WAIT_STATUS_TIMEOUT"
wait_pod_gone "$target" 120
wait_ready_replicas 3 "$WAIT_READY_TIMEOUT"
t1="$(now_s)"
dt=$((t1-t0))
info "Graceful deletion elapsed=${dt}s"
[ "$dt" -ge 15 ] && ok "Graceful deletion was slow enough (>=15s)" || fail "Graceful deletion was too fast (elapsed=${dt}s) — expected >=15s"

echo "=== TEST 3: CANCEL AFTER FORCEFUL (ensure delayed graceful didn't run) ==="
delete_fi_if_exists "$FI_CANCEL"
apply_file_strict "$PODFAULTS_MANIFEST"
assert_fi_exists_or_debug "$FI_CANCEL"

target="$(pick_one_pod)"
info "Target baseline pod: $target"

wait_podfault_action_succeeded "$FI_CANCEL" "forceful-oneshot" "$WAIT_STATUS_TIMEOUT"
wait_pod_gone "$target" 60
wait_ready_replicas 3 "$WAIT_READY_TIMEOUT"

cancel_fi "$FI_CANCEL"
assert_no_action_tick_exists "$FI_CANCEL" "graceful-delayed-window" 90

echo "✅ ALL PODFAULT ONE_SHOT + CANCELLATION TESTS PASSED (ns=$NS)"
