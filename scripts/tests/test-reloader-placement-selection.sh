#!/usr/bin/env bash
# Exercise the real CI step: unrelated merge groups must not pull a Reloader chart.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf -- "$scratch"' EXIT
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
step="$(yq -er '.jobs.validate-helm-post-renderers.steps[] | select(.name == "🧩 Verify independent reloader placement") | .run' "$root_dir/.github/workflows/ci.yaml")"
mkdir -p "$scratch/bin" "$scratch/repo/scripts/tests"
printf '#!/usr/bin/env bash\nexit 0\n' >"$scratch/bin/shellcheck"
chmod +x "$scratch/bin/shellcheck"
cat >"$scratch/repo/scripts/tests/test-reloader-rendered-placement.sh" <<'STUB'
#!/usr/bin/env bash
printf 'chart-rendered\n' >> "$RENDER_RECEIPT"
STUB
git -C "$scratch/repo" init -q
git -C "$scratch/repo" add scripts/tests/test-reloader-rendered-placement.sh
git -C "$scratch/repo" -c user.name=fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false commit -qm base
base="$(git -C "$scratch/repo" rev-parse HEAD)"
# Make a real commit and run the workflow's shell body against its actual diff.
run_case() {
  local name=$1 path=$2 expected=$3 result=0 receipt="$scratch/receipt"
  git -C "$scratch/repo" -c advice.detachedHead=false checkout -q --detach "$base"
  mkdir -p "$scratch/repo/$(dirname "$path")"
  printf '# selection fixture\n' >>"$scratch/repo/$path"
  git -C "$scratch/repo" add -- "$path"
  git -C "$scratch/repo" -c user.name=fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false commit -qm "$name"
  rm -f "$receipt"
  (cd "$scratch/repo" && PATH="$scratch/bin:$PATH" BASE_SHA="$base" RENDER_RECEIPT="$receipt" bash -e -o pipefail -c "$step") >"$scratch/output" 2>&1 || result=$?
  [[ "$result" == 0 ]] || fail "$name: workflow step failed"
  if [[ "$expected" == render ]]; then
    if [[ ! -f "$receipt" ]] || [[ "$(wc -l <"$receipt")" -ne 1 ]]; then
      fail "$name: relevant change was not rendered exactly once"
    fi
  else
    [[ ! -e "$receipt" ]] || fail "$name: unrelated change pulled the chart"
  fi
  printf 'PASS: %s\n' "$name"
}
run_case docs-only README.md skip
run_case unrelated-application k8s/bases/apps/whoami/helm-release.yaml skip
run_case unrelated-controller k8s/bases/infrastructure/controllers/longhorn/helm-release.yaml skip
run_case reloader-values k8s/bases/infrastructure/controllers/reloader/helm-release.yaml render
run_case chart-source k8s/bases/infrastructure/controllers/reloader/helm-repository.yaml render
run_case production-composition k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml render
run_case local-composition k8s/providers/docker/infrastructure/controllers/kustomization.yaml render
run_case shared-component k8s/bases/components/helmrelease-flux-defaults/kustomization.yaml render
run_case replica-config k8s/clusters/prod/bootstrap/config-map.yaml render
run_case kubernetes-profile ksail.prod.yaml render
run_case chart-test scripts/tests/test-reloader-rendered-placement.sh render
run_case audited-sdk scripts/render-helm-chart/go.mod render
run_case workflow .github/workflows/ci.yaml render
rm -f "$scratch/receipt"
if (cd "$scratch/repo" && PATH="$scratch/bin:$PATH" BASE_SHA=missing-revision RENDER_RECEIPT="$scratch/receipt" bash -e -o pipefail -c "$step") >"$scratch/output" 2>&1; then
  fail 'unreadable base was accepted'
fi
[[ ! -e "$scratch/receipt" ]] || fail 'unreadable base reached chart execution'
printf 'PASS: unreadable base fails closed\n'
