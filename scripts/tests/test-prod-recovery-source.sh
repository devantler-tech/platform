#!/usr/bin/env bash
# Exercise the real promotion command with an advancing recovery baseline.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$work_dir/bin" "$work_dir/checkout"
FIXTURE_GIT="$(command -v git)"
export FIXTURE_GIT
git -C "$work_dir/checkout" init -q
printf 'validated\n' > "$work_dir/checkout/source.txt"
git -C "$work_dir/checkout" add source.txt
git -C "$work_dir/checkout" -c user.name=Fixture -c user.email=fixture@example.invalid \
  -c commit.gpgsign=false commit -qm baseline
baseline="$(git -C "$work_dir/checkout" rev-parse HEAD)"
git -C "$work_dir/checkout" -c user.name=Fixture -c user.email=fixture@example.invalid \
  -c commit.gpgsign=false commit --allow-empty -qm newer-main
newer="$(git -C "$work_dir/checkout" rev-parse HEAD)"
git -C "$work_dir/checkout" checkout -q --detach "$baseline"
printf '%s\n' "$newer" > "$work_dir/main-sha"
cat > "$work_dir/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE_DIR/api-calls"
sha="$(cat "$FIXTURE_DIR/main-sha")"
case "${FIXTURE_API_MODE:-normal}" in
  malformed) printf '{"ref":'; exit 0 ;;
  empty) exit 0 ;;
  absent) printf '{}\n'; exit 0 ;;
  wrong-ref) printf '{"ref":"refs/heads/other","object":{"type":"commit","sha":"%s"}}\n' "$sha"; exit 0 ;;
  wrong-type) printf '{"ref":"refs/heads/main","object":{"type":"tag","sha":"%s"}}\n' "$sha"; exit 0 ;;
  duplicate) printf '{"ref":"refs/heads/main","object":{"type":"commit","sha":"%s"}}\n' "$sha" ;;
  change-checkout) git -C "$GITHUB_WORKSPACE" checkout -q --detach "$FIXTURE_NEWER" ;;
esac
printf '{"ref":"refs/heads/main","object":{"type":"commit","sha":"%s"}}\n' "$sha"
exit "${FIXTURE_API_EXIT:-0}"
GH
cat > "$work_dir/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${FIXTURE_GIT_FAIL_DIFF:-0}" == 1 && " $* " == *' diff '* ]]; then exit 128; fi
exec "$FIXTURE_GIT" "$@"
GIT
cat > "$work_dir/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$3" in
  create) printf 'promoted\n' >> "$FIXTURE_DIR/promotions" ;;
  inspect) printf '%s\n' "$STAGING_DIGEST" ;;
  *) exit 42 ;;
esac
DOCKER
chmod +x "$work_dir/bin/gh" "$work_dir/bin/docker" "$work_dir/bin/git"
export FIXTURE_NEWER="$newer"
export FIXTURE_DIR="$work_dir"
export PATH="$work_dir/bin:$PATH"
export GITHUB_WORKSPACE="$work_dir/checkout" GITHUB_REPOSITORY=devantler-tech/platform
export GH_TOKEN=fixture-token RECOVERY_SOURCE_SHA="$baseline"
export SUBJECT_NAME=ghcr.io/devantler-tech/platform/manifests
STAGING_DIGEST="sha256:$(printf '%064d' 1)"
export STAGING_DIGEST GITHUB_OUTPUT="$work_dir/output"
yq -r '.runs.steps[] | select(.id == "promote_latest") | .run' \
  "$root_dir/.github/actions/deploy-prod/publish-platform-manifests/action.yml" > "$work_dir/promote.sh"
cd "$root_dir"
if bash "$work_dir/promote.sh" > "$work_dir/log" 2>&1; then
  echo 'FAIL: an outdated recovery checkout promoted the production artifact' >&2
  exit 1
fi
if [[ -e "$work_dir/promotions" ]]; then
  echo 'FAIL: stale recovery reached the promotion command' >&2
  exit 1
fi
grep -q 'PROD_RECOVERY_SOURCE=STALE' "$work_dir/log" || {
  echo 'FAIL: recovery did not fail for the observed stale main revision' >&2
  cat "$work_dir/log" >&2
  exit 1
}
printf 'PASS: stale recovery cannot promote production\n'

yq -r '.runs.steps[] | select(.id == "verify_recovery_source") | .run' "$root_dir/.github/actions/deploy-prod/action.yml" > "$work_dir/entry.sh"
entry_status=0
bash "$work_dir/entry.sh" > "$work_dir/log" 2>&1 || entry_status=$?
if [[ "$entry_status" != 1 ]] || ! grep -q 'PROD_RECOVERY_SOURCE=STALE' "$work_dir/log"; then
  echo 'FAIL: outdated recovery reached the deployment entry point' >&2; exit 1
fi
printf 'PASS: stale recovery stops before deployment tooling and production changes\n'

printf '%s\n' "$baseline" > "$work_dir/main-sha"
check_promotion() {
  local want="$1" label="$2" status=0
  rm -f "$work_dir/promotions" "$work_dir/output"
  bash "$work_dir/promote.sh" > "$work_dir/log" 2>&1 || status=$?
  [[ "$status" == "$want" ]] || { echo "FAIL: $label returned $status, wanted $want" >&2; cat "$work_dir/log" >&2; exit 1; }
  if [[ "$want" == 0 ]]; then
    [[ "$(cat "$work_dir/promotions")" == promoted ]] || { echo "FAIL: $label never promoted" >&2; exit 1; }
  else
    [[ ! -e "$work_dir/promotions" ]] || { echo "FAIL: $label promoted rejected bytes" >&2; exit 1; }
  fi
  printf 'PASS: %s\n' "$label"
}
check_promotion 0 'unchanged validated main promotes'
export GITHUB_SHA="$newer"
check_promotion 0 'event SHA cannot replace the recorded recovery checkout'
unset GITHUB_SHA
[[ "$(head -1 "$work_dir/api-calls")" == 'api --hostname github.com --method GET repos/devantler-tech/platform/git/ref/heads/main' ]] || { echo 'FAIL: freshness read escaped the fixed main endpoint' >&2; exit 1; }
for mode in malformed empty absent wrong-ref wrong-type duplicate; do
  export FIXTURE_API_MODE="$mode"
  check_promotion 2 "incomplete $mode response is UNKNOWN"
done
export FIXTURE_API_MODE=normal FIXTURE_API_EXIT=42
check_promotion 2 'plausible output from a failed API is UNKNOWN'
export FIXTURE_API_EXIT=124
check_promotion 2 'timed-out read is UNKNOWN'
export FIXTURE_API_EXIT=0 FIXTURE_API_MODE=change-checkout
check_promotion 1 'checkout changed during freshness read is stale'
git -C "$work_dir/checkout" checkout -q --detach "$baseline"
export FIXTURE_API_MODE=normal
printf 'changed\n' > "$work_dir/checkout/source.txt"
check_promotion 1 'dirty validated checkout is stale'
git -C "$work_dir/checkout" restore source.txt
printf 'changed\n' > "$work_dir/checkout/source.txt"
git -C "$work_dir/checkout" add source.txt
check_promotion 1 'staged changes to the validated checkout are stale'
git -C "$work_dir/checkout" restore --staged --worktree source.txt
export FIXTURE_GIT_FAIL_DIFF=1
check_promotion 2 'failed checkout inspection is UNKNOWN'
unset FIXTURE_GIT_FAIL_DIFF
export GIT_DIR="$work_dir/nonexistent" GIT_WORK_TREE="$work_dir/nonexistent" GIT_COMMON_DIR="$work_dir/nonexistent"
check_promotion 0 'inherited Git paths cannot redirect the checkout read'
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR
export RECOVERY_SOURCE_SHA=not-a-commit
check_promotion 2 'invalid validated revision is UNKNOWN'
export RECOVERY_SOURCE_SHA="$baseline" GITHUB_REPOSITORY=other/example
check_promotion 2 'foreign repository cannot supply a recovery receipt'
export GITHUB_REPOSITORY=devantler-tech/platform
unset GH_TOKEN
check_promotion 2 'missing bounded reader credential is UNKNOWN'
export RECOVERY_SOURCE_SHA=''
rm -f "$work_dir/api-calls"
check_promotion 0 'speculative publication retains its existing path'
[[ ! -e "$work_dir/api-calls" ]] || { echo 'FAIL: speculative publication performed a main freshness read' >&2; exit 1; }
printf 'PASS: complete recovery freshness behavior\n'
