#!/usr/bin/env bash
# Run the reviewed independent publication fixtures with a bounded worker pool.
# Serial is the default; CI opts into two workers on its existing runner.
set -euo pipefail
umask 077
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
driver="$root/scripts/run-publication-generator-tests.sh"
tests=(
  test-publish-workflow-approved-revisions.sh
  test-tenant-publication-binding.sh
  test-bootstrap-tenant-source.sh
  test-canonical-source-bootstrap.sh
  test-canonical-source-upgrade.sh
  test-canonical-constructor-attribution.sh
  test-canonical-generator-output.sh
  test-canonical-bootstrap-output.sh
  test-canonical-publisher-regeneration.sh
)
die() { printf 'publication-tests: %s\n' "$*" >&2; exit 2; }

# xargs appends only one validated index; it never chooses a command or path.
# Capture all child exit codes, including 255, without telling xargs to stop
# scheduling the remaining fixtures. The parent aggregates every result.
if [[ ${1:-} == --worker ]]; then
  [[ $# == 3 && $2 == /* && -d $2 && ! -L $2 ]] || die 'invalid worker directory'
  valid_index=0
  for index in "${!tests[@]}"; do
    [[ "$3" != "$index" ]] || valid_index=1
  done
  [[ "$valid_index" == 1 ]] || die 'invalid worker index'
  worker_work="$2"
  worker_index="$3"
  worker_status=0
  SECONDS=0
  bash "$root/scripts/tests/${tests[$worker_index]}" >"$worker_work/$worker_index.log" 2>&1 || worker_status=$?
  printf '%s %s\n' "$worker_status" "$SECONDS" >"$worker_work/$worker_index.status"
  exit 0
fi

workers=1
if [[ $# != 0 ]]; then
  [[ $# == 2 && $1 == --jobs ]] || die 'usage: run-publication-generator-tests.sh [--jobs 1|2]'
  case "$2" in 1|2) workers="$2" ;; *) die 'worker limit must be 1 or 2' ;; esac
fi
command -v xargs >/dev/null 2>&1 || die 'xargs is required'
for test_file in "${tests[@]}"; do
  [[ -f "$root/scripts/tests/$test_file" && ! -L "$root/scripts/tests/$test_file" ]] || die "missing regular test: $test_file"
done
work="$(mktemp -d)" || die 'cannot create the private results directory'
trap 'rm -rf "$work"' EXIT
SECONDS=0
printf 'PUBLICATION_TESTS start jobs=%s count=%s\n' "$workers" "${#tests[@]}"
dispatch_status=0
printf '%s\0' "${!tests[@]}" |
  xargs -0 -n 1 -P "$workers" bash "$driver" --worker "$work" || dispatch_status=$?
failed=0
for index in "${!tests[@]}"; do
  [[ -f "$work/$index.status" && -f "$work/$index.log" ]] || die "no complete result for ${tests[$index]}"
  read -r result seconds <"$work/$index.status" || die "unreadable result for ${tests[$index]}"
  [[ "$result" =~ ^[0-9]+$ && "$seconds" =~ ^[0-9]+$ ]] || die "invalid result for ${tests[$index]}"
  printf '::group::%s (exit %s, %ss)\n' "${tests[$index]}" "$result" "$seconds"
  cat "$work/$index.log" || die "cannot read output for ${tests[$index]}"
  printf '::endgroup::\n'
  [[ "$result" == 0 ]] || failed=$((failed+1))
done
[[ "$dispatch_status" == 0 ]] || die 'worker dispatch failed'
printf 'PUBLICATION_TESTS jobs=%s count=%s failed=%s seconds=%s\n' "$workers" "${#tests[@]}" "$failed" "$SECONDS"
[[ "$failed" == 0 ]]
