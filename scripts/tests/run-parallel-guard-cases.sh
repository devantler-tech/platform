#!/usr/bin/env bash
# Run independent filesystem guard fixtures with bounded parallelism.

set -uo pipefail

die() {
  printf 'run-parallel-guard-cases: %s\n' "$*" >&2
  exit 2
}

load_expectations() { # <wants-file> <label>; fills the worker's expected array
  local wants="$1" label="$2" want
  expected=()
  if [ ! -f "$wants" ] || [ ! -r "$wants" ] || [ ! -s "$wants" ]; then
    printf 'FAIL %s: invalid expected output: missing, unreadable or empty wants file\n' "$label" >&2
    return 1
  fi
  while :; do
    want=''
    if IFS= read -r -d '' want; then
      if [ -z "$want" ]; then
        printf 'FAIL %s: invalid expected output: empty marker\n' "$label" >&2
        return 1
      fi
      expected+=("$want")
    else
      if [ -n "$want" ]; then
        printf 'FAIL %s: invalid expected output: unterminated marker\n' "$label" >&2
        return 1
      fi
      break
    fi
  done <"$wants" || {
    printf 'FAIL %s: invalid expected output: cannot read wants file\n' "$label" >&2
    return 1
  }
  if [ "${#expected[@]}" -eq 0 ]; then
    printf 'FAIL %s: invalid expected output: no markers\n' "$label" >&2
    return 1
  fi
}

run_worker() { # <guard> <case-dir>
  local guard="$1" case_dir="$2" kind label root out rc=0 want missing=''
  local -a expected
  kind="$(cat "$case_dir/kind")" || die "cannot read $case_dir/kind"
  label="$(cat "$case_dir/label")" || die "cannot read $case_dir/label"
  root="$(cat "$case_dir/root")" || die "cannot read $case_dir/root"
  out="$root.out"
  load_expectations "$case_dir/wants" "$label" || return 1

  "$guard" "$root" >"$out" 2>&1 || rc=$?
  case "$kind" in
    pass)
      if [ "$rc" -ne 0 ]; then
        printf 'FAIL %s: exited %s on an agreeing tree: %s\n' "$label" "$rc" "$(cat "$out")" >&2
        return 1
      fi
      for want in "${expected[@]}"; do
        grep -qF -- "$want" "$out" || missing="$missing '$want'"
      done
      if [ -n "$missing" ]; then
        printf 'FAIL %s: exited 0 without expected output%s: %s\n' "$label" "$missing" "$(cat "$out")" >&2
        return 1
      fi
      ;;
    refusal)
      if [ "$rc" -eq 0 ]; then
        printf 'FAIL %s: exited 0, so the case was not refused: %s\n' "$label" "$(cat "$out")" >&2
        return 1
      fi
      for want in "${expected[@]}"; do
        grep -qF -- "$want" "$out" || missing="$missing '$want'"
      done
      if [ -n "$missing" ]; then
        printf 'FAIL %s: refused for the wrong reason; missing%s: %s\n' "$label" "$missing" "$(cat "$out")" >&2
        return 1
      fi
      ;;
    *) die "unknown case kind '$kind' in $case_dir" ;;
  esac

  printf 'ok   %s\n' "$label"
}

if [ "${1:-}" = '--worker' ]; then
  [ "$#" -eq 3 ] || die 'worker usage: --worker <guard> <case-dir>'
  run_worker "$2" "$3"
  exit $?
fi

[ "$#" -eq 3 ] || die 'usage: <guard> <cases-dir> <parallel-workers>'
guard="$1"
cases_dir="$2"
workers="$3"
[ -x "$guard" ] || die "guard '$guard' is not executable"
[ -d "$cases_dir" ] || die "cases directory '$cases_dir' does not exist"
case "$workers" in
  '' | *[!0-9]*) die 'parallel-workers must be a positive integer' ;;
esac
[ "$workers" -gt 0 ] || die 'parallel-workers must be a positive integer'

self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
shopt -s nullglob
case_dirs=("$cases_dir"/*)
shopt -u nullglob
[ "${#case_dirs[@]}" -gt 0 ] || die "cases directory '$cases_dir' is empty"

pids=()
next_wait=0
failures=0
for case_dir in "${case_dirs[@]}"; do
  "$self" --worker "$guard" "$case_dir" &
  pids+=("$!")
  if [ "$((${#pids[@]} - next_wait))" -ge "$workers" ]; then
    wait "${pids[$next_wait]}" || failures=$((failures + 1))
    next_wait=$((next_wait + 1))
  fi
done

while [ "$next_wait" -lt "${#pids[@]}" ]; do
  wait "${pids[$next_wait]}" || failures=$((failures + 1))
  next_wait=$((next_wait + 1))
done

[ "$failures" -eq 0 ]
