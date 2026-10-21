#!/usr/bin/env bash
# Dynamic CEL identities must not make existing low-ID findings disappear.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
[[ "$(trivy --version | sed -n 's/^Version: //p' | head -n 1)" == 0.74.0 ]] || { echo 'FAIL: require reviewed Trivy 0.74.0' >&2; exit 1; }
bash "$root/scripts/tests/test-webapp-inputs-wiring.sh"
PLATFORM_COMPILED_WEBAPP_DIR="$work/compiled" bash "$root/scripts/tests/test-webapp-compiler.sh" >"$work/compiler.log" 2>&1 || { cat "$work/compiler.log" >&2; exit 1; }
mkdir "$work/controls"
cp "$work/compiled/"*.json "$work/controls/"
jq '.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem=false' "$work/compiled/enabled.json" >"$work/controls/writable.json"
jq '.spec.template.spec.securityContext.runAsUser=1000' "$work/compiled/enabled.json" >"$work/controls/low-user.json"
jq '.spec.template.spec.securityContext.runAsGroup=1000' "$work/compiled/enabled.json" >"$work/controls/low-group.json"
(
  cd "$work/controls"
  TRIVY_CACHE_DIR="$work/cache" trivy fs --scanners misconfig --skip-check-update \
    --ignorefile /dev/null --format json --output "$work/results.json" --config-data "$root/.trivy/data" . 2>"$work/trivy.log"
)
grep -Fq 'Falling back to embedded checks' "$work/trivy.log"
jq -e '
  [.Results[] | {key:.Target,value:([.Misconfigurations[]? | {ID,Severity}]|sort_by(.ID))}] | from_entries |
  . == {
    "enabled.json":[],
    "omitted.json":[{ID:"KSV-0014",Severity:"HIGH"},{ID:"KSV-0020",Severity:"LOW"},{ID:"KSV-0021",Severity:"LOW"}],
    "writable.json":[{ID:"KSV-0014",Severity:"HIGH"}],
    "low-user.json":[{ID:"KSV-0020",Severity:"LOW"}],
    "low-group.json":[{ID:"KSV-0021",Severity:"LOW"}]
  }' "$work/results.json" >/dev/null || { echo 'FAIL: compiled WebApp findings changed or a resource was not scanned' >&2; exit 1; }
echo 'PASS: actual compiled workloads retain default writable-root/low-ID findings; enabled workload is clean and three regressions are detected.'
