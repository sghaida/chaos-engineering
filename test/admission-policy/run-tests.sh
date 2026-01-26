#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
USECASES_DIR="${ROOT_DIR}/usecases"

NS="${NS:-demo}"
MODE="${MODE:-dry-run}"                  # dry-run | apply
KUBECTL="${KUBECTL:-kubectl}"
FAIL_FAST="${FAIL_FAST:-0}"              # 1 = stop on first failure
SHOW_OUTPUT_ON_PASS="${SHOW_OUTPUT_ON_PASS:-0}"  # 1 = show kubectl output even for passes

if [[ "$MODE" == "dry-run" ]]; then
  APPLY_CMD=("$KUBECTL" apply --server-side --dry-run=server -f)
else
  APPLY_CMD=("$KUBECTL" apply -f)
fi

pass=0
fail=0

finish() {
  echo
  echo "Summary: $pass passed, $fail failed (total: $((pass+fail)))"
}
trap finish EXIT

echo "Running admission policy tests"
echo "  Root:      $ROOT_DIR"
echo "  Usecases:  $USECASES_DIR"
echo "  Namespace: $NS"
echo "  Mode:      $MODE"
echo

# ---- discover files ----------------------------------------------------------
if [[ ! -d "$USECASES_DIR" ]]; then
  echo "ERROR: usecases directory not found: $USECASES_DIR"
  exit 1
fi

shopt -s nullglob
files=("$USECASES_DIR"/*.yaml)
if [[ ${#files[@]} -eq 0 ]]; then
  echo "ERROR: No test YAML files found in $USECASES_DIR"
  exit 1
fi

# ---- preflight (never abort) -------------------------------------------------
set +e
"$KUBECTL" version --client >/dev/null 2>&1
kubectl_ok=$?
"$KUBECTL" get validatingadmissionpolicy faultinjection.chaos.sghaida.io >/dev/null 2>&1
vap_ok=$?
"$KUBECTL" get validatingadmissionpolicybinding faultinjection.chaos.sghaida.io >/dev/null 2>&1
vapb_ok=$?
set -e

if [[ $kubectl_ok -ne 0 ]]; then
  echo "ERROR: kubectl not working (check kubeconfig / cluster connectivity)."
  exit 1
fi
if [[ $vap_ok -ne 0 ]]; then
  echo "WARN: validatingadmissionpolicy faultinjection.chaos.sghaida.io not found. Tests may all ALLOW."
fi
if [[ $vapb_ok -ne 0 ]]; then
  echo "WARN: validatingadmissionpolicybinding faultinjection.chaos.sghaida.io not found. Tests may all ALLOW."
fi
echo

# ---- run tests ---------------------------------------------------------------
for f in "${files[@]}"; do
  base="$(basename "$f")"
  echo "→ $base"

  # Parse expectations safely without grep/pipefail killing the script.
  expect="$(sed -nE 's/^# EXPECT: *//p' "$f" | head -n1)"
  expect_msg="$(sed -nE 's/^# EXPECT_MSG_CONTAINS: *//p' "$f" | head -n1)"

  if [[ -z "${expect:-}" ]]; then
    printf "❌ %-55s (%s)\n" "$base" "MISSING EXPECT HEADER"
    echo "   ---- output ----"
    echo "   File must contain: '# EXPECT: ALLOW' or '# EXPECT: DENY'"
    echo "   ---------------"
    fail=$((fail+1))
    [[ "$FAIL_FAST" == "1" ]] && exit 1
    continue
  fi

  # Run kubectl and capture exit code correctly (DENY must not abort script)
  set +e
  out="$("${APPLY_CMD[@]}" "$f" 2>&1)"
  rc=$?
  set -e

  ok=false
  if [[ "$expect" == "ALLOW" ]]; then
    [[ $rc -eq 0 ]] && ok=true
  else
    if [[ $rc -ne 0 ]]; then
      if [[ -z "${expect_msg:-}" ]]; then
        ok=true
      else
        # Support multiple required substrings separated by " || "
        ok=true
        IFS='||' read -r -a parts <<< "$expect_msg"
        for p in "${parts[@]}"; do
          needle="$(echo "$p" | sed 's/^ *//; s/ *$//')"
          if [[ -n "$needle" ]] && ! echo "$out" | grep -Fq "$needle"; then
            ok=false
            break
          fi
        done
      fi
    fi
  fi

  if [[ "$ok" == "true" ]]; then
    printf "✅ %-55s (%s)\n" "$base" "$expect"
    pass=$((pass+1))
    if [[ "$SHOW_OUTPUT_ON_PASS" == "1" ]]; then
      echo "$out" | sed 's/^/   /'
    fi
  else
    printf "❌ %-55s (%s)\n" "$base" "$expect"
    echo "   ---- output ----"
    echo "$out" | sed 's/^/   /'
    echo "   ---------------"
    fail=$((fail+1))
    [[ "$FAIL_FAST" == "1" ]] && exit 1
  fi
done

# Exit non-zero if any failures
[[ $fail -eq 0 ]]
