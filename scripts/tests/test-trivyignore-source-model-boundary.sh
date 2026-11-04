#!/usr/bin/env bash
# Native paired controls for fingerprint-bound ConfigMaps and the bootstrap LimitRange.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
engine="${TRIVY_NATIVE_BIN:-$(command -v trivy || true)}"
case "$engine" in /*) ;; *) echo 'Native Trivy 0.74.0 is required' >&2; exit 2 ;; esac
version="$("$engine" --version)"
[ "${version%%$'\n'*}" = 'Version: 0.74.0' ] || { echo 'Native Trivy 0.74.0 is required' >&2; exit 2; }
for tool in go yq jq; do command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }; done
if [ -n "${BOUNDARY_EVIDENCE_DIR:-}" ]; then
  work="$(mktemp -d "${BOUNDARY_EVIDENCE_DIR%/}/trivy-source-boundary.XXXXXX")"
else
  work="$(mktemp -d)"
  trap 'rm -rf -- "$work"' EXIT
fi
umask 077
cd "$root"
go build -o "$work/premise" ./scripts/guard-trivy-source-dispositions
"$work/premise" --root "$root"
go test ./internal/trivydispositions
mkdir -p "$work/cache" "$work/bootstrap" "$work/tree"
cat > "$work/bootstrap/quota.yaml" <<'YAML'
apiVersion: v1
kind: ResourceQuota
metadata:
  name: complete-quota
spec:
  hard:
    requests.cpu: "1"
    requests.memory: 1Gi
    limits.cpu: "2"
    limits.memory: 2Gi
YAML
native() (
  # Match the production policy guard: no TRIVY_* setting may change these controls.
  while IFS= read -r name; do case "$name" in TRIVY_*) unset "$name" ;; esac; done < <(compgen -e)
  "$engine" --cache-dir "$work/cache" --debug "$@"
)
native config --timeout 2m --include-non-failures --format json --exit-code 0 "$work/bootstrap" > "$work/bootstrap.json" 2> "$work/bootstrap.stderr"
# The existing real-engine guard verifies the complete official inventory and
# runs its quota/unrelated-finding controls. No reduced/embedded policy is accepted.
PLATFORM_TRIVY_CONTROL_ENGINE="$engine" PLATFORM_TRIVY_CONTROL_CACHE="$work/cache" \
  go test -count=1 -run '^TestNativeQuotaAndUnrelatedFindingControls$' ./scripts/guard-trivy-policy
cp "$root/.trivyignore.yaml" "$work/tree/.trivyignore.yaml"
homepage='k8s/bases/apps/homepage/config-map.yaml'
progress='k8s/bases/infrastructure/coroot/components/security-progress/config-map.yaml'
limits='k8s/bases/bootstrap/limit-ranges/kube-system.yaml'
for path in "$homepage" "$progress" "$limits"; do
  mkdir -p "$work/tree/$(dirname "$path")"
  cp "$root/$path" "$work/tree/$path"
done
probe_home='k8s/bases/apps/trivy-source-homepage/config-map.yaml'
probe_progress='k8s/bases/apps/trivy-source-progress/config-map.yaml'
probe_limits='k8s/bases/apps/trivy-source-limits/limit-range.yaml'
application='k8s/bases/apps/trivy-source-application/pod.yaml'
for path in "$probe_home" "$probe_progress" "$probe_limits" "$application"; do mkdir -p "$work/tree/$(dirname "$path")"; done
cp "$work/tree/$homepage" "$work/tree/$probe_home"
cp "$work/tree/$progress" "$work/tree/$probe_progress"
cp "$work/tree/$limits" "$work/tree/$probe_limits"
cmp "$work/tree/$homepage" "$work/tree/$probe_home"
cmp "$work/tree/$progress" "$work/tree/$probe_progress"
cmp "$work/tree/$limits" "$work/tree/$probe_limits"
cat > "$work/tree/$application" <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: application-workload
  namespace: kube-system
spec:
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: application
      image: registry.k8s.io/pause:3.10
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      resources:
        requests: {cpu: 5m, memory: 8Mi}
        limits: {cpu: 10m, memory: 16Mi}
YAML
touch "$work/empty-ignore"
scan() {
  local name="$1" ignore="$2" status=0
  (cd "$work/tree" && native fs --timeout 2m --scanners misconfig --skip-check-update --ignorefile "$ignore" --format json --exit-code 23 .) > "$work/$name.json" 2> "$work/$name.stderr" || status=$?
  [ "$status" -eq 23 ] || { echo "Native $name scan did not retain finding exit23 (got $status)" >&2; exit 1; }
}
count() { jq -er --arg path "$2" --arg id "$3" '[.Results[]? | select(.Target == $path) | .Misconfigurations[]? | select(.ID == $id)] | length' "$work/$1.json"; }
expect() {
  local actual
  actual="$(count "$1" "$2" "$3")"
  [ "$actual" -eq "$4" ] || { echo "Native $1/$2/$3 expected $4 findings, got $actual" >&2; exit 1; }
}
scan baseline "$work/empty-ignore"
scan scoped .trivyignore.yaml
for pair in "$homepage|$probe_home|KSV-01010" "$progress|$probe_progress|KSV-01010" "$progress|$probe_progress|KSV-0109" "$limits|$probe_limits|KSV-0037"; do
  IFS='|' read -r listed unlisted id <<< "$pair"
  expect baseline "$listed" "$id" 1
  expect baseline "$unlisted" "$id" 1
  expect scoped "$listed" "$id" 0
  expect scoped "$unlisted" "$id" 1
done
expect baseline "$application" KSV-0037 1
expect scoped "$application" KSV-0037 1
"$work/premise" --root "$work/tree"
# Adding actual credential-shaped data at a listed path must invalidate the
# premise; an unlisted identical copy must remain a native positive control.
for pair in "$homepage|$probe_home" "$progress|$probe_progress"; do
  IFS='|' read -r listed unlisted <<< "$pair"
  yq -i '.data.password = "synthetic-boundary-secret"' "$work/tree/$listed"
  cp "$work/tree/$listed" "$work/tree/$unlisted"
  if "$work/premise" --root "$work/tree" > "$work/refusal.stdout" 2> "$work/refusal.stderr"; then
    echo "Changed ConfigMap retained its disposition: $listed" >&2; exit 1
  fi
  cp "$root/$listed" "$work/tree/$listed"
  "$work/premise" --root "$work/tree"
done
for path in "$homepage" "$progress"; do yq -i '.data.password = "synthetic-boundary-secret"' "$work/tree/$path"; done
scan secret-no-ignore "$work/empty-ignore"
scan secret-scoped .trivyignore.yaml
for path in "$homepage" "$progress"; do expect secret-no-ignore "$path" KSV-0109 1; done
for path in "$probe_home" "$probe_progress"; do expect secret-scoped "$path" KSV-0109 1; done
echo 'PASS: four native dispositions are exact-path scoped; identical unlisted inputs and kube-system application remain detected; secret additions invalidate the fingerprint and remain native positives.'
if [ -n "${BOUNDARY_EVIDENCE_DIR:-}" ]; then printf 'Private native evidence: %s\n' "$work"; fi
