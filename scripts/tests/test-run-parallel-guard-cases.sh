#!/usr/bin/env bash
# RED/GREEN contract for run-parallel-guard-cases.sh (#4569).

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
runner="$repo_root/scripts/tests/run-parallel-guard-cases.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

make_case() { # <id> <kind> <root> <expected>...
  local id="$1" kind="$2" root="$3" case_dir
  case_dir="$scratch/cases/$id"
  shift 3
  mkdir -p "$case_dir" "$root"
  printf '%s\n' "$kind" >"$case_dir/kind"
  printf '%s\n' "case $id" >"$case_dir/label"
  printf '%s\n' "$root" >"$case_dir/root"
  printf '%s\0' "$@" >"$case_dir/wants"
}

mkdir -p "$scratch/cases" "$scratch/state"

fake_guard="$scratch/fake-guard.sh"
cat >"$fake_guard" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$1"
state="${PARALLEL_GUARD_TEST_STATE:?}"
name="$(basename "$root")"
touch "$state/$name.started"
for _ in $(seq 1 100); do
  started="$(find "$state" -name '*.started' -type f | wc -l | tr -d ' ')"
  [ "$started" -ge 4 ] && break
  sleep 0.02
done
[ "$started" -ge 4 ] || {
  printf 'workers did not overlap\n' >&2
  exit 90
}
case "$name" in
  refuse-*)
    printf 'expected refusal for %s\n' "$name"
    exit 1
    ;;
  *)
    printf 'expected pass for %s\n' "$name"
    ;;
esac
EOF
chmod +x "$fake_guard"

make_case 01 pass "$scratch/pass-one" 'expected pass'
make_case 02 refusal "$scratch/refuse-one" 'expected refusal'
make_case 03 pass "$scratch/pass-two" 'expected pass'
make_case 04 refusal "$scratch/refuse-two" 'expected refusal'

export PARALLEL_GUARD_TEST_STATE="$scratch/state"
if ! output="$(bash "$runner" "$fake_guard" "$scratch/cases" 4 2>&1)"; then
  printf 'parallel cases unexpectedly failed:\n%s\n' "$output" >&2
  exit 1
fi
for id in 01 02 03 04; do
  grep -qF "ok   case $id" <<<"$output" || {
    printf 'parallel output omitted case %s:\n%s\n' "$id" "$output" >&2
    exit 1
  }
done

# Negative controls exercise the worker verdicts rather than only the scheduler.
cp -R "$scratch/cases" "$scratch/missing-want"
printf '%s\0' 'not emitted' >"$scratch/missing-want/01/wants"
if bash "$runner" "$fake_guard" "$scratch/missing-want" 4 >"$scratch/missing.out" 2>&1; then
  printf 'missing expected output was accepted\n' >&2
  exit 1
fi
grep -qF 'without expected output' "$scratch/missing.out" || {
  printf 'missing-output refusal was not diagnostic\n' >&2
  exit 1
}

cp -R "$scratch/cases" "$scratch/wrong-kind"
printf '%s\n' refusal >"$scratch/wrong-kind/01/kind"
if bash "$runner" "$fake_guard" "$scratch/wrong-kind" 4 >"$scratch/kind.out" 2>&1; then
  printf 'a passing guard was accepted as a refusal\n' >&2
  exit 1
fi
grep -qF 'exited 0, so the case was not refused' "$scratch/kind.out" || {
  printf 'wrong-kind refusal was not diagnostic\n' >&2
  exit 1
}

if bash "$runner" "$fake_guard" "$scratch/cases" 0 >"$scratch/jobs.out" 2>&1; then
  printf 'zero parallel workers were accepted\n' >&2
  exit 1
fi
grep -qF 'positive integer' "$scratch/jobs.out" || {
  printf 'invalid-worker refusal was not diagnostic\n' >&2
  exit 1
}

printf 'parallel guard-case runner contract passed\n'
