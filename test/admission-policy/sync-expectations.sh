#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
USECASES_DIR="${ROOT_DIR}/usecases"

NS="${NS:-demo}"
KUBECTL="${KUBECTL:-kubectl}"
MODE="${MODE:-dry-run}" # dry-run only (we always use server dry-run)
DRYRUN_ARGS=(apply --server-side --dry-run=server -f)

# Max length for stored error snippet (keep headers readable)
MAX_MSG_LEN="${MAX_MSG_LEN:-220}"

# If true, keep existing EXPECT_MSG_CONTAINS and append " || <new>"
APPEND="${APPEND:-0}"

# Extract a stable, useful substring from kubectl error output
# - prefer the admission policy "message" if present
# - else keep the most relevant first line
extract_msg() {
  local out="$1"
  local msg=""

  # Try to extract Kubernetes Status.message if it appears
  # (kubectl often prints: "Error from server: <message>")
  msg="$(echo "$out" | sed -nE 's/^Error from server: *//p' | head -n1)"

  # If not found, fall back to first non-empty line
  if [[ -z "$msg" ]]; then
    msg="$(echo "$out" | sed '/^[[:space:]]*$/d' | head -n1)"
  fi

  # Trim + cap length
  msg="$(echo "$msg" | sed -E 's/[[:space:]]+/ /g' | sed -E 's/^ +| +$//g')"
  if [[ ${#msg} -gt $MAX_MSG_LEN ]]; then
    msg="${msg:0:$MAX_MSG_LEN}"
  fi

  echo "$msg"
}

update_file() {
  local file="$1"
  local expect
  expect="$(sed -nE 's/^# EXPECT: *//p' "$file" | head -n1 | tr -d '\r')"

  # Only update DENY cases
  if [[ "$expect" != "DENY" ]]; then
    return 0
  fi

  # Run server dry-run apply
  set +e
  out="$("$KUBECTL" -n "$NS" "${DRYRUN_ARGS[@]}" "$file" 2>&1)"
  rc=$?
  set -e

  # If kubectl succeeded but file expects DENY, we won't override automatically
  # (that usually indicates a real regression)
  if [[ $rc -eq 0 ]]; then
    echo "SKIP (expected DENY but got ALLOW): $(basename "$file")"
    return 0
  fi

  local msg
  msg="$(extract_msg "$out")"

  if [[ -z "$msg" ]]; then
    echo "WARN: could not extract message for $(basename "$file")"
    return 0
  fi

  local new_header="# EXPECT_MSG_CONTAINS: $msg"

  # If header exists
  if grep -qE '^# EXPECT_MSG_CONTAINS:' "$file"; then
    if [[ "$APPEND" == "1" ]]; then
      # Append only if it's not already present
      local cur
      cur="$(sed -nE 's/^# EXPECT_MSG_CONTAINS: *//p' "$file" | head -n1)"
      if echo "$cur" | grep -Fq "$msg"; then
        echo "OK (already contains msg): $(basename "$file")"
        return 0
      fi
      # Append with " || "
      local merged="# EXPECT_MSG_CONTAINS: ${cur} || ${msg}"
      # Replace the first occurrence only
      perl -0777 -i -pe 's/^# EXPECT_MSG_CONTAINS:.*$/'"$(printf '%s' "$merged" | sed 's/[\/&]/\\&/g')"'/m' "$file"
      echo "UPDATED (append): $(basename "$file")"
      return 0
    else
      # Replace the first occurrence only
      perl -0777 -i -pe 's/^# EXPECT_MSG_CONTAINS:.*$/'"$(printf '%s' "$new_header" | sed 's/[\/&]/\\&/g')"'/m' "$file"
      echo "UPDATED: $(basename "$file")"
      return 0
    fi
  fi

  # If header does not exist: insert it after # EXPECT: line if possible, else at top
  if grep -qE '^# EXPECT:' "$file"; then
    perl -0777 -i -pe 's/^(# EXPECT:.*\n)/$1'"$(printf '%s\n' "$new_header" | sed 's/[\/&]/\\&/g')"'/m' "$file"
  else
    # not expected, but be safe
    perl -0777 -i -pe 's/^/'"$(printf '%s\n' "$new_header" | sed 's/[\/&]/\\&/g')"'/m' "$file"
  fi

  echo "UPDATED (insert): $(basename "$file")"
}

main() {
  if [[ ! -d "$USECASES_DIR" ]]; then
    echo "ERROR: usecases directory not found: $USECASES_DIR"
    exit 1
  fi

  shopt -s nullglob
  local files=("$USECASES_DIR"/*.yaml)
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "ERROR: no YAML files found in $USECASES_DIR"
    exit 1
  fi

  echo "Syncing EXPECT_MSG_CONTAINS for DENY tests"
  echo "  Namespace: $NS"
  echo "  Usecases:  $USECASES_DIR"
  echo "  Append:    $APPEND"
  echo

  for f in "${files[@]}"; do
    update_file "$f"
  done

  echo
  echo "Done."
}

main
