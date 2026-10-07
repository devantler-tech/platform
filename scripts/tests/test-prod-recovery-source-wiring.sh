#!/usr/bin/env bash
# Keep every current-main recovery bound to its validated revision.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
guard="if [[ -n \"\${RECOVERY_SOURCE_SHA}\" ]]; then
  bash scripts/verify-prod-recovery-source.sh \"\${RECOVERY_SOURCE_SHA}\"
fi"
violation() {
  local root="$1" deploy publisher ci cd dr promote first create ci_runs
  deploy="$root/.github/actions/deploy-prod/action.yml"
  publisher="$root/.github/actions/deploy-prod/publish-platform-manifests/action.yml"
  ci="$root/.github/workflows/ci.yaml"
  cd="$root/.github/workflows/cd.yaml"
  dr="$root/.github/workflows/dr-rebuild.yaml"
  [[ "$(yq -r '.inputs.recovery-source-sha.default' "$deploy")" == '' &&
     "$(yq -r '.inputs.recovery-source-sha.default' "$publisher")" == '' ]] || { echo speculative-default; return; }
  [[ "$(yq -r '.runs.steps[0].id' "$deploy")" == verify_recovery_source ]] || { echo deploy-first-gate; return; }
  [[ "$(yq -r '.runs.steps[0].run' "$deploy")" == "$guard" ]] || { echo deploy-command; return; }
  [[ "$(yq -r '.runs.steps[0] | has("if")' "$deploy")" == false &&
     "$(yq -r '.runs.steps[0] | has("continue-on-error")' "$deploy")" == false ]] || { echo deploy-unconditional; return; }
  [[ "$(yq -r '.runs.steps[0].env.RECOVERY_SOURCE_SHA' "$deploy")" == "\${{ inputs.recovery-source-sha }}" ]] || { echo deploy-binding; return; }
  [[ "$(yq -r '.runs.steps[0].env.GH_TOKEN' "$deploy")" == "\${{ github.token }}" ]] || { echo reader-token; return; }
  [[ "$(yq -r '.runs.steps[] | select(.id == "publish_platform_manifest") | .with.recovery-source-sha' "$deploy")" == "\${{ inputs.recovery-source-sha }}" ]] || { echo publisher-binding; return; }
  [[ "$(yq -r '.jobs.heal-prod-on-failure.steps[] | select(.uses == "./.github/actions/deploy-prod") | .with.recovery-source-sha' "$ci")" == "\${{ steps.recovery-baseline.outputs.sha }}" ]] || { echo heal-binding; return; }
  [[ "$(yq -r '.jobs.deploy-prod.steps[] | select(.uses == "./.github/actions/deploy-prod") | .with.recovery-source-sha // ""' "$ci")" == '' ]] || { echo speculative-publication; return; }
  [[ "$(yq -r '.jobs.deploy-prod.steps[] | select(.uses == "./.github/actions/deploy-prod") | .with.recovery-source-sha' "$cd")" == "\${{ github.sha }}" ]] || { echo manual-binding; return; }
  [[ "$(yq -r '.jobs.rebuild.steps[] | select(.id == "publish_platform_manifest") | .with.recovery-source-sha' "$dr")" == "\${{ github.sha }}" ]] || { echo rebuild-binding; return; }
  first="$(yq -r '.jobs.rebuild.steps | to_entries | map(select(.value.id == "verify_recovery_source")) | .[0].key // -1' "$dr")"
  create="$(yq -r '.jobs.rebuild.steps | to_entries | map(select(.value.run == "./scripts/run-ksail-prod-with-pull-auth.sh cluster create")) | .[0].key // -1' "$dr")"
  [[ "$first" -ge 0 && "$create" -gt "$first" ]] || { echo rebuild-before-create; return; }
  [[ "$(yq -r '.jobs.rebuild.steps[] | select(.id == "verify_recovery_source") | .run' "$dr")" == "bash scripts/verify-prod-recovery-source.sh \"\${RECOVERY_SOURCE_SHA}\"" ]] || { echo rebuild-command; return; }
  [[ "$(yq -r '.jobs.rebuild.steps[] | select(.id == "verify_recovery_source") | .env.RECOVERY_SOURCE_SHA' "$dr")" == "\${{ github.sha }}" ]] || { echo rebuild-source; return; }
  [[ "$(yq -r '.jobs.rebuild.steps[] | select(.id == "verify_recovery_source") | .env.GH_TOKEN' "$dr")" == "\${{ github.token }}" ]] || { echo rebuild-reader; return; }
  [[ "$(yq -r '.jobs.rebuild.steps[] | select(.id == "verify_recovery_source") | has("if")' "$dr")" == false &&
     "$(yq -r '.jobs.rebuild.steps[] | select(.id == "verify_recovery_source") | has("continue-on-error")' "$dr")" == false ]] || { echo rebuild-unconditional; return; }
  [[ "$(yq -r '.runs.steps[] | select(.id == "promote_latest") | .env.RECOVERY_SOURCE_SHA' "$publisher")" == "\${{ inputs.recovery-source-sha }}" ]] || { echo promotion-binding; return; }
  [[ "$(yq -r '.runs.steps[] | select(.id == "promote_latest") | .env.GH_TOKEN' "$publisher")" == "\${{ github.token }}" ]] || { echo promotion-token; return; }
  promote="$(yq -r '.runs.steps[] | select(.id == "promote_latest") | .run' "$publisher")"
  [[ "$promote" == "set -euo pipefail
$guard
"* ]] || { echo late-guard; return; }
  ci_runs="$(yq -r '.jobs.changes.steps[].run // ""' "$ci")"
  grep -Fxq 'bash scripts/tests/test-prod-recovery-source.sh' <<< "$ci_runs" || { echo behavioral-ci; return; }
  grep -Fxq 'bash scripts/tests/test-prod-recovery-source-wiring.sh' <<< "$ci_runs" || { echo wiring-ci; return; }
}
got="$(violation "$root_dir")"
[[ -z "$got" ]] || { echo "FAIL: recovery wiring violates $got" >&2; exit 1; }
for file in .github/actions/deploy-prod/action.yml .github/actions/deploy-prod/publish-platform-manifests/action.yml .github/workflows/ci.yaml .github/workflows/cd.yaml .github/workflows/dr-rebuild.yaml; do
  mkdir -p "$work_dir/base/$(dirname "$file")"
  cp "$root_dir/$file" "$work_dir/base/$file"
done
ablate() {
  local want="$1" file="$2" mutation="$3" copy="$work_dir/$1" got
  cp -R "$work_dir/base" "$copy"
  yq -i "$mutation" "$copy/$file"
  got="$(violation "$copy")"
  [[ "$got" == "$want" ]] || { echo "FAIL: $want mutation returned ${got:-nothing}" >&2; exit 1; }
  printf 'PASS: %s ablation\n' "$want"
}
ablate deploy-first-gate .github/actions/deploy-prod/action.yml 'del(.runs.steps[0])'
ablate speculative-default .github/actions/deploy-prod/action.yml '.inputs.recovery-source-sha.default = "main"'
ablate deploy-unconditional .github/actions/deploy-prod/action.yml '.runs.steps[0].continue-on-error = true'
ablate reader-token .github/actions/deploy-prod/action.yml '.runs.steps[0].env.GH_TOKEN = "wrong"'
ablate publisher-binding .github/actions/deploy-prod/action.yml 'del(.runs.steps[] | select(.id == "publish_platform_manifest") | .with.recovery-source-sha)'
ablate heal-binding .github/workflows/ci.yaml 'del(.jobs.heal-prod-on-failure.steps[] | select(.uses == "./.github/actions/deploy-prod") | .with.recovery-source-sha)'
ablate manual-binding .github/workflows/cd.yaml 'del(.jobs.deploy-prod.steps[] | select(.uses == "./.github/actions/deploy-prod") | .with.recovery-source-sha)'
ablate rebuild-binding .github/workflows/dr-rebuild.yaml 'del(.jobs.rebuild.steps[] | select(.id == "publish_platform_manifest") | .with.recovery-source-sha)'
ablate rebuild-reader .github/workflows/dr-rebuild.yaml '(.jobs.rebuild.steps[] | select(.id == "verify_recovery_source") | .env.GH_TOKEN) = "wrong"'
ablate rebuild-unconditional .github/workflows/dr-rebuild.yaml '(.jobs.rebuild.steps[] | select(.id == "verify_recovery_source") | .if) = "false"'
ablate late-guard .github/actions/deploy-prod/publish-platform-manifests/action.yml '(.runs.steps[] | select(.id == "promote_latest") | .run) = "true"'
printf 'PASS: current-main recovery wiring and all ablations\n'
