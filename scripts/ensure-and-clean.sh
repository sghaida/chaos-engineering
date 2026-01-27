#!/usr/bin/env bash
set -euo pipefail

NS="${NS:-demo}"
FI_RULE_PREFIX="${FI_RULE_PREFIX:-fi-}"

# Resolve script dir even when invoked from another working directory
SCRIPT_PATH="${BASH_SOURCE[0]:-$0}"
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"

# Default layout:
# k8s-test/
#   resources/
#   fi-operator/
#     scripts/ensure-and-clean.sh
DEFAULT_RES_DIR="$(cd "$SCRIPT_DIR/../../resources" 2>/dev/null && pwd || true)"

# RES_DIR can be overridden by env or by --resources-dir
RES_DIR="${RES_DIR:-${DEFAULT_RES_DIR:-}}"

ok()   { printf "✅ %s\n" "$1"; }
info() { printf "ℹ️  %s\n" "$1"; }
warn() { printf "⚠️  %s\n" "$1"; }
fail() { printf "❌ %s\n" "$1"; exit 1; }

usage() {
  cat <<EOF
Usage:
  $(basename "$0") [--resources-dir PATH] [--namespace NS]

Env overrides:
  NS=demo
  RES_DIR=/path/to/resources
  FI_RULE_PREFIX=fi-

Default RES_DIR (if not provided):
  $SCRIPT_DIR/../../resources
EOF
}

# -------- args --------
while [ "${1:-}" != "" ]; do
  case "$1" in
    --resources-dir|-r)
      shift
      RES_DIR="${1:-}"
      ;;
    --namespace|-n)
      shift
      NS="${1:-}"
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      fail "Unknown arg: $1 (use --help)"
      ;;
  esac
  shift || true
done

[ -n "${RES_DIR:-}" ] || fail "RES_DIR not set and default layout not found"
[ -d "$RES_DIR" ] || fail "Resources directory not found: $RES_DIR (use --resources-dir PATH or RES_DIR=...)"

info "Using namespace: $NS"
info "Using resources:  $RES_DIR"

wait_no_fi_rules() {
  local timeout_s="${1:-120}"
  local step_s="${2:-2}"
  local i=0

  info "Waiting for injected rules to be removed (prefix=${FI_RULE_PREFIX})..."
  while [ "$i" -lt "$timeout_s" ]; do
    h1="$(kubectl -n "$NS" get vs httpbin1 -o jsonpath="{range .spec.http[*]}{.name}{\"\n\"}{end}" 2>/dev/null || true)"
    h2="$(kubectl -n "$NS" get vs httpbin2 -o jsonpath="{range .spec.http[*]}{.name}{\"\n\"}{end}" 2>/dev/null || true)"

    if printf "%s\n%s\n" "$h1" "$h2" | grep -q "^${FI_RULE_PREFIX}"; then
      sleep "$step_s"
      i=$((i+step_s))
      continue
    fi

    ok "No injected rules found in VirtualServices"
    return 0
  done

  warn "Timed out waiting for injected rules to be removed"
}

ensure_namespace() {
  if kubectl get ns "$NS" >/dev/null 2>&1; then
    ok "Namespace $NS exists"
  else
    info "Creating namespace $NS"
    kubectl create ns "$NS" >/dev/null
    ok "Namespace $NS created"
  fi
}

cleanup_faultinjection_state() {
  info "Cleaning up FaultInjection CRs (controller should revert VS rules)"
  kubectl -n "$NS" delete faultinjection --all --ignore-not-found >/dev/null || true
  wait_no_fi_rules 120 2 || true
  ok "FaultInjection cleanup done"
}

# newline separated list (no mapfile)
list_files() {
  local dir="$1"; shift
  local pat f
  for pat in "$@"; do
    for f in "$dir"/$pat; do
      [ -e "$f" ] || continue
      printf "%s\n" "$f"
    done
  done
}

apply_group() {
  local label="$1"
  local files_nl="$2"

  [ -n "$files_nl" ] || { warn "No $label yamls found (skipping)"; return 0; }

  info "Applying $label:"
  printf "%s\n" "$files_nl" | while IFS= read -r f; do
    [ -n "$f" ] || continue
    printf "  - %s\n" "$f"
  done

  # Apply each file explicitly (simple + robust)
  printf "%s\n" "$files_nl" | while IFS= read -r f; do
    [ -n "$f" ] || continue
    kubectl apply -f "$f" >/dev/null
  done

  ok "$label applied"
}

main() {
  ensure_namespace

  # 1️⃣ Cleanup
  cleanup_faultinjection_state

  # 2️⃣ Validation policy FIRST
  VALIDATION="$(list_files "$RES_DIR" "*validation*policy*.y*ml" || true)"
  apply_group "validation-policy" "$VALIDATION"

  # 3️⃣ Workloads
  WORKLOADS="$(list_files "$RES_DIR" "*workload*.y*ml" "*deployment*.y*ml" "*service*.y*ml" || true)"
  apply_group "workloads" "$WORKLOADS"

  # 4️⃣ VirtualServices
  VS="$(list_files "$RES_DIR" "*vs*.y*ml" "*virtualservice*.y*ml" || true)"
  apply_group "virtualservices" "$VS"

  # 5️⃣ FaultInjection scenario (optional)
  FI="$(list_files "$RES_DIR" "*fi*scenario*.y*ml" "*faultinject*.y*ml" || true)"
  apply_group "faultinjection scenario" "$FI"

  info "Sanity checks"
  kubectl -n "$NS" get pods >/dev/null && ok "Pods reachable"
  kubectl -n "$NS" get vs >/dev/null && ok "VirtualServices reachable"

  ok "Environment is clean, validated, and ready"
}

main "$@"
