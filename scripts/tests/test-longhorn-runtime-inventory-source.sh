#!/usr/bin/env bash
# Exercise source selection with two reviewed revisions in a disposable Git repository.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin"
upstream="$fixture/upstream"
export GITHUB_WORKSPACE="$fixture/checkout"

git init -q -b main "$upstream"
git -C "$upstream" config user.name Fixture
git -C "$upstream" config user.email fixture@example.invalid
git -C "$upstream" config commit.gpgsign false
mkdir -p "$upstream/internal/runtime" "$upstream/.github/workflows"
printf 'original library\n' >"$upstream/internal/runtime/input.txt"
printf 'original workflow\n' >"$upstream/.github/workflows/prepare-longhorn-v2-node.yaml"
git -C "$upstream" add -- internal/runtime/input.txt .github/workflows/prepare-longhorn-v2-node.yaml
git -C "$upstream" commit -qm baseline
original="$(git -C "$upstream" rev-parse HEAD)"
git clone -q --depth=1 "file://$upstream" "$GITHUB_WORKSPACE"
git -C "$GITHUB_WORKSPACE" remote set-url origin https://github.com/devantler-tech/platform.git
git -C "$GITHUB_WORKSPACE" config "url.$upstream.insteadOf" https://github.com/devantler-tech/platform.git
git -C "$GITHUB_WORKSPACE" switch -q --detach "$original"

cat >"$fixture/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
main="$(git -C "$FIXTURE_UPSTREAM" rev-parse refs/heads/main)"
case "${!#}" in
  repos/devantler-tech/platform/git/ref/heads/main)
    printf '{"ref":"refs/heads/main","object":{"type":"commit","sha":"%s"}}\n' "$main"
    ;;
  "repos/devantler-tech/platform/commits/$main")
    printf '{"sha":"%s","commit":{"verification":{"verified":%s}}}\n' "$main" "${FIXTURE_VERIFIED:-true}"
    ;;
  *) exit 42 ;;
esac
GH
chmod +x "$fixture/bin/gh"
export PATH="$fixture/bin:$PATH" FIXTURE_UPSTREAM="$upstream"
export GITHUB_ACTIONS=true GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF=refs/heads/main
export GITHUB_REPOSITORY=devantler-tech/platform GITHUB_RUN_ATTEMPT=1 GITHUB_SHA="$original"
export LONGHORN_V2_OPERATION=inventory-v1-runtime LONGHORN_V2_CONFIRM=inventory-v1-runtime
export GH_TOKEN=fixture-token

select_source() {
  bash "$root/.github/scripts/select-longhorn-runtime-inventory-source.sh"
}

must_refuse() {
  local label="$1"
  if select_source >"$fixture/log" 2>&1; then
    printf 'FAIL: accepted %s\n' "$label" >&2
    exit 1
  fi
  [[ "$(git -C "$GITHUB_WORKSPACE" rev-parse HEAD)" == "$original" ]]
  printf 'PASS: refused %s without changing checkout\n' "$label"
}

[[ "$(select_source)" == "$original" ]]
printf 'Unrelated documentation\n' >"$upstream/README.md"
git -C "$upstream" add -- README.md
git -C "$upstream" commit -qm unrelated
current="$(git -C "$upstream" rev-parse HEAD)"
[[ "$(select_source)" == "$current" ]]
[[ "$(git -C "$GITHUB_WORKSPACE" rev-parse HEAD)" == "$current" ]]
printf 'PASS: current reviewed source selected when all inventory inputs match\n'
git -C "$GITHUB_WORKSPACE" switch -q --detach "$original"

export FIXTURE_VERIFIED=false
must_refuse "an unverified main commit"
unset FIXTURE_VERIFIED
export LONGHORN_V2_OPERATION=retain-provider-source
must_refuse "a storage mutation operation"
export LONGHORN_V2_OPERATION=inventory-v1-runtime
printf 'Local state\n' >"$GITHUB_WORKSPACE/local.txt"
must_refuse "a dirty runner checkout"
rm "$GITHUB_WORKSPACE/local.txt"

printf 'changed library\n' >"$upstream/internal/runtime/input.txt"
git -C "$upstream" add -- internal/runtime/input.txt
git -C "$upstream" commit -qm library-change
must_refuse "changed inventory dependencies"

git -C "$upstream" restore --source="$original" internal/runtime/input.txt
printf 'changed workflow\n' >"$upstream/.github/workflows/prepare-longhorn-v2-node.yaml"
git -C "$upstream" add -- internal/runtime/input.txt .github/workflows/prepare-longhorn-v2-node.yaml
git -C "$upstream" commit -qm workflow-change
must_refuse "a changed protected workflow"

same_tree_without_history="$(git -C "$upstream" commit-tree "$original^{tree}" -m unrelated-history)"
git -C "$upstream" update-ref refs/heads/main "$same_tree_without_history"
must_refuse "a main revision outside the tested history"
