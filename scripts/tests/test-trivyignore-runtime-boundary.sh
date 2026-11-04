#!/usr/bin/env bash
# Full native scanners: exact runtime exceptions, unlisted twins, unknown checks.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
engine="${TRIVY_NATIVE_BIN:-$(command -v trivy || true)}"
checkov="${CHECKOV_NATIVE_BIN:-$(command -v checkov || true)}"
case "$engine" in /*) ;; *)
  echo 'Native Trivy 0.74.0 is required' >&2
  exit 2
  ;;
esac
case "$checkov" in /*) ;; *)
  echo 'Native Checkov 3.3.15 is required' >&2
  exit 2
  ;;
esac
version="$("$engine" --version)"
[ "${version%%$'\n'*}" = 'Version: 0.74.0' ] || {
  echo 'Native Trivy 0.74.0 is required' >&2
  exit 2
}
[ "$("$checkov" --version)" = '3.3.15' ] || {
  echo 'Native Checkov 3.3.15 is required' >&2
  exit 2
}
for tool in go kubectl kyverno yq jq; do command -v "$tool" >/dev/null || {
  echo "$tool is required" >&2
  exit 2
}; done
kyverno_version="$(kyverno version)"
[ "${kyverno_version%%$'\n'*}" = 'Version: 1.19.2' ] || {
  echo 'Native Kyverno 1.19.2 is required' >&2
  exit 2
}
umask 077
if [ -n "${BOUNDARY_EVIDENCE_DIR:-}" ]; then
  work="$(mktemp -d "${BOUNDARY_EVIDENCE_DIR%/}/trivy-runtime-boundary.XXXXXX")"
else
  work="$(mktemp -d)"
  trap 'rm -rf -- "$work"' EXIT
fi
cd "$root"
RUNTIME_TOOLS_NATIVE_TESTS=1 bash scripts/tests/test-runtime-disposition-tools.sh
go build -o "$work/premise" ./scripts/guard-trivy-runtime-dispositions
"$work/premise" --root "$root"
go test -count=1 ./internal/trivyruntimedispositions
PLATFORM_RUNTIME_NATIVE_CONTROLS=1 go test -count=1 -run '^TestNativeWorldProviderOverlayControls$' ./internal/trivyruntimedispositions
# These existing tests execute the actual consumer script and native Kyverno
# mutations, including convergence and near misses; their inputs are pinned.
for test in prune-protected-orphan-alert csi-sidecar-baseline-context longhorn-csi-plugin-baseline-context longhorn-engine-image-baseline-context longhorn-registrar-prestop-policy; do
  bash "scripts/tests/test-$test.sh"
done
mkdir -p "$work/cache" "$work/bootstrap" "$work/tree"
printf '{}\n' >"$work/checkov.yaml"
cat >"$work/bootstrap/quota.yaml" <<'YAML'
apiVersion: v1
kind: ResourceQuota
metadata: {name: complete-quota}
spec:
  hard:
    requests.cpu: "1"
    requests.memory: 1Gi
    limits.cpu: "2"
    limits.memory: 2Gi
YAML
native() (
  while IFS= read -r name; do case "$name" in TRIVY_*) unset "$name" ;; esac done < <(compgen -e)
  "$engine" --cache-dir "$work/cache" --debug "$@"
)
native config --timeout 2m --include-non-failures --format json --exit-code 0 "$work/bootstrap" >"$work/bootstrap.json" 2>"$work/bootstrap.stderr"
PLATFORM_TRIVY_CONTROL_ENGINE="$engine" PLATFORM_TRIVY_CONTROL_CACHE="$work/cache" \
  go test -count=1 -run '^TestNativeQuotaAndUnrelatedFindingControls$' ./scripts/guard-trivy-policy
cp "$root/.trivyignore.yaml" "$work/tree/.trivyignore.yaml"
paths=(
  k8s/bases/infrastructure/controllers/kyverno/BackgroundDownstreamCleanup.Dockerfile
  k8s/bases/infrastructure/controllers/kyverno/MigrationRuntime.Dockerfile
  k8s/bases/infrastructure/controllers/kyverno/ReportsWatchPlan.Dockerfile
  k8s/bases/infrastructure/coroot/CorootCheckpoint.Dockerfile
  k8s/bases/infrastructure/coroot/CorootClusterAgentCheckpoint.Dockerfile
  k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile
  k8s/bases/apps/private-mail/stateful-set.yaml
  k8s/bases/infrastructure/coroot/components/security-progress/deployment.yaml
  k8s/bases/infrastructure/vault-backup/cron-job.yaml
  k8s/bases/infrastructure/vault-backup/job.yaml
  k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role.yaml
  k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-csi-sidecars.yaml
  k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-longhorn-csi-plugin.yaml
  k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-longhorn-engine-image.yaml
  k8s/providers/hetzner/apps/world-at-ruin/role-operator.yaml
)
for path in "${paths[@]}"; do
  twin="native-control/$path"
  mkdir -p "$work/tree/$(dirname "$path")" "$work/tree/$(dirname "$twin")"
  cp "$root/$path" "$work/tree/$path"
  cp "$root/$path" "$work/tree/$twin"
  cmp "$work/tree/$path" "$work/tree/$twin"
done
touch "$work/empty-ignore"
scan() {
  local name="$1" ignore="$2" status=0
  (cd "$work/tree" && native fs --timeout 2m --scanners misconfig --skip-check-update --ignorefile "$ignore" --format json --exit-code 23 .) >"$work/$name.json" 2>"$work/$name.stderr" || status=$?
  [ "$status" -eq 23 ] || {
    echo "Native $name scan must preserve finding exit23 (got $status)" >&2
    exit 1
  }
}
expect() {
  local actual
  actual="$(jq -er --arg path "$2" --arg id "$3" '[.Results[]? | select(.Target == $path) | .Misconfigurations[]? | select(.ID == $id)] | length' "$work/$1.json")"
  [ "$actual" -eq "$4" ] || {
    echo "$1/$2/$3 expected $4, got $actual" >&2
    exit 1
  }
}
scan baseline "$work/empty-ignore"
scan scoped .trivyignore.yaml
for path in "${paths[@]}"; do
  case "$path" in
  *Dockerfile) ids='DS-0026' ;;
  *private-mail*) ids='KSV-0020 KSV-0021 KSV-0022' ;;
  *prune-protected-orphan-alert/role.yaml) ids='KSV-0049' ;;
  *longhorn/role-kyverno-mutate-*) ids='KSV-0048' ;;
  *world-at-ruin/role-operator.yaml) ids='KSV-0053' ;;
  *) ids='KSV-0036' ;;
  esac
  case "$path" in *CorootNodeAgent.Dockerfile) ids="$ids DS-0002" ;; esac
  for id in $ids; do
    expect baseline "$path" "$id" 1
    expect baseline "native-control/$path" "$id" 1
    expect scoped "$path" "$id" 0
    expect scoped "native-control/$path" "$id" 1
  done
done
# Checkov annotations are resource-scoped. Unannotated twins still fail every
# reviewed check, and an unrelated insecure field still fails on a listed object.
mail='k8s/bases/apps/private-mail/stateful-set.yaml'
node='k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile'
for name in baseline annotated; do
  mkdir -p "$work/checkov-$name/listed" "$work/checkov-$name/unlisted"
  cp "$root/$mail" "$work/checkov-$name/listed/mail.yaml"
  cp "$root/$mail" "$work/checkov-$name/unlisted/mail.yaml"
  cp "$root/$node" "$work/checkov-$name/listed/Dockerfile"
  cp "$root/$node" "$work/checkov-$name/unlisted/Dockerfile"
  yq -i 'del(.metadata.annotations)' "$work/checkov-$name/unlisted/mail.yaml"
  sed '/# checkov:skip=/d' "$root/$node" >"$work/checkov-$name/unlisted/Dockerfile"
done
cp "$work/checkov-baseline/unlisted/mail.yaml" "$work/checkov-baseline/listed/mail.yaml"
cp "$work/checkov-baseline/unlisted/Dockerfile" "$work/checkov-baseline/listed/Dockerfile"
yq -i '.spec.template.spec.hostNetwork = true' "$work/checkov-annotated/listed/mail.yaml"
checkov_native() (
  while IFS= read -r name; do case "$name" in CKV_* | BC_API_KEY | PRISMA_API_URL | BRIDGECREW_API_KEY) unset "$name" ;; esac done < <(compgen -e)
  cd "$1"
  "$checkov" --config-file "$work/checkov.yaml" --skip-download --download-external-modules false --framework kubernetes dockerfile --directory . --quiet --output json
)
for name in baseline annotated; do
  status=0
  checkov_native "$work/checkov-$name" >"$work/checkov-$name.json" 2>"$work/checkov-$name.stderr" || status=$?
  [ "$status" -eq 1 ] || {
    echo "Native Checkov $name must preserve finding exit1 (got $status)" >&2
    exit 1
  }
  jq -e '[if type == "array" then .[] else . end | .results.parsing_errors[]?] | length == 0' "$work/checkov-$name.json" >/dev/null
done
checkov_count() {
  jq -er --arg path "$2" --arg id "$3" '[if type == "array" then .[] else . end | .results.failed_checks[]? | select(.file_path == $path and .check_id == $id)] | length' "$work/checkov-$1.json"
}
for pair in 'mail.yaml|CKV_K8S_25' 'mail.yaml|CKV_K8S_35' 'mail.yaml|CKV_K8S_40' 'Dockerfile|CKV_DOCKER_3'; do
  IFS='|' read -r file id <<<"$pair"
  [ "$(checkov_count baseline "/listed/$file" "$id")" -eq 1 ]
  [ "$(checkov_count baseline "/unlisted/$file" "$id")" -eq 1 ]
  [ "$(checkov_count annotated "/listed/$file" "$id")" -eq 0 ]
  [ "$(checkov_count annotated "/unlisted/$file" "$id")" -eq 1 ]
done
[ "$(checkov_count annotated /listed/mail.yaml CKV_K8S_19)" -eq 1 ]
# The annotation alone would suppress a new secret/capability: the mandatory
# premise guard's separate mutation controls refuse those changes.
"$work/premise" --root "$root"
echo 'PASS: native exact-path Trivy and Checkov annotation pairs retain unlisted/unknown controls; image/identity/ENV/token/RBAC mutation guards pass. Docker execution is not claimed.'
if [ -n "${BOUNDARY_EVIDENCE_DIR:-}" ]; then printf 'Private native evidence: %s\n' "$work"; fi
