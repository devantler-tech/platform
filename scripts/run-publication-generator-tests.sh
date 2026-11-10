#!/usr/bin/env bash
# Run the reviewed independent publication fixtures with a bounded worker pool.
# Serial and complete by default. CI sets PUBLICATION_SUITE to one fixed partition.
set -euo pipefail
umask 077
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
driver="$root/scripts/run-publication-generator-tests.sh"
tests=(
  upgrade:test-publish-workflow-approved-revisions.sh
  upgrade:test-tenant-publication-binding.sh
  upgrade:test-bootstrap-tenant-source.sh
  bootstrap:test-canonical-source-bootstrap.sh
  upgrade:test-canonical-source-upgrade.sh
  bootstrap:test-canonical-constructor-attribution.sh
  bootstrap:test-canonical-generator-output.sh
  bootstrap:test-canonical-bootstrap-output.sh
  bootstrap:test-canonical-publisher-regeneration.sh
)
die() {
  printf 'publication-tests: %s\n' "$*" >&2
  exit 2
}

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
  test_file="${tests[$worker_index]#*:}"
  worker_status=0
  SECONDS=0
  bash "$root/scripts/tests/$test_file" >"$worker_work/$worker_index.log" 2>&1 || worker_status=$?
  printf '%s %s\n' "$worker_status" "$SECONDS" >"$worker_work/$worker_index.status"
  exit 0
fi

workers=1
if [[ $# != 0 ]]; then
  [[ $# == 2 && $1 == --jobs ]] || die 'usage: run-publication-generator-tests.sh [--jobs 1|2|4]'
  case "$2" in 1 | 2 | 4) workers="$2" ;; *) die 'worker limit must be 1, 2 or 4' ;; esac
fi
command -v xargs >/dev/null 2>&1 || die 'xargs is required'
suite="${PUBLICATION_SUITE-all}"
case "$suite" in all | bootstrap | upgrade) ;; *) die 'invalid publication suite' ;; esac
selected=()
for index in "${!tests[@]}"; do
  entry="${tests[$index]}"
  test_file="${entry#*:}"
  [[ -f "$root/scripts/tests/$test_file" && ! -L "$root/scripts/tests/$test_file" ]] || die "missing regular test: $test_file"
  if [[ "$suite" == all || "$suite" == "${entry%%:*}" ]]; then
    selected+=("$index")
  fi
done
[[ -n "${selected[0]:-}" ]] || die 'publication suite has no tests'
work="$(mktemp -d)" || die 'cannot create the private results directory'
trap 'rm -rf "$work"' EXIT
SECONDS=0
printf 'PUBLICATION_TESTS start jobs=%s count=%s\n' "$workers" "${#selected[@]}"
dispatch_status=0
printf '%s\0' "${selected[@]}" |
  xargs -0 -n 1 -P "$workers" bash "$driver" --worker "$work" || dispatch_status=$?
failed=0
for index in "${selected[@]}"; do
  test_file="${tests[$index]#*:}"
  [[ -f "$work/$index.status" && -f "$work/$index.log" ]] || die "no complete result for $test_file"
  read -r result seconds <"$work/$index.status" || die "unreadable result for $test_file"
  [[ "$result" =~ ^[0-9]+$ && "$seconds" =~ ^[0-9]+$ ]] || die "invalid result for $test_file"
  printf '::group::%s (exit %s, %ss)\n' "$test_file" "$result" "$seconds"
  cat "$work/$index.log" || die "cannot read output for $test_file"
  printf '::endgroup::\n'
  [[ "$result" == 0 ]] || failed=$((failed + 1))
done
[[ "$dispatch_status" == 0 ]] || die 'worker dispatch failed'
printf 'PUBLICATION_TESTS jobs=%s count=%s failed=%s seconds=%s\n' "$workers" "${#selected[@]}" "$failed" "$SECONDS"
[[ "$failed" == 0 ]]
