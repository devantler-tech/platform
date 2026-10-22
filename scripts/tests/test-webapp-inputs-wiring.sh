#!/usr/bin/env bash
# Exercise the actual platform publication scanner, not a copied validator call.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
# Inject generator failures into the actual runner, retaining the real compiled
# validator. The Go shim preserves -C semantics without rebuilding it per case.
real_jq="$(command -v jq)"
go -C "$root" build -o "$work/validator" ./scripts/validate-webapp-inputs
mkdir "$work/bin"
cat >"$work/bin/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ -e "$WEBAPP_FIXTURE_FAILED" ]]; then : >"$WEBAPP_FIXTURE_VALIDATED"; exit 90; fi
[[ "$#" -eq 7 && "$1" == -C && "$2" == "$WEBAPP_FIXTURE_ROOT" &&
   "$3" == run && "$4" == ./scripts/validate-webapp-inputs && "$5" == json &&
   "$7" == "$WEBAPP_FIXTURE_ROOT/scripts/webapp-inputs.jq" ]] || exit 90
cd -- "$2"
shift 4
exec "$WEBAPP_FIXTURE_VALIDATOR" "$@"
SH
cat >"$work/bin/jq" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$#" -eq 1 && "$1" == "$WEBAPP_FIXTURE_MUTATION" ]]; then
  : >"$WEBAPP_FIXTURE_FAILED"
  exit 42
fi
exec "$WEBAPP_FIXTURE_JQ" "$@"
SH
chmod 0700 "$work/bin/go" "$work/bin/jq"
failed=0
control=0
for mutation in \
  '.spec.args = null' \
  '.spec.args=["é" * 128]' \
  '.spec.args=[range(0;32)|("x" * 256)]' \
  '.unexpected="PRIVATE-FIXTURE-VALUE"'; do
  control=$((control + 1))
  if PATH="$work/bin:$PATH" WEBAPP_FIXTURE_ROOT="$root" \
    WEBAPP_FIXTURE_VALIDATOR="$work/validator" WEBAPP_FIXTURE_JQ="$real_jq" \
    WEBAPP_FIXTURE_FAILED="$work/failed-$control" WEBAPP_FIXTURE_VALIDATED="$work/validated-$control" \
    WEBAPP_FIXTURE_MUTATION="$mutation" bash "$root/scripts/tests/test-webapp-inputs.sh" >"$work/mutation.log" 2>&1; then
    echo "FAIL: generator failure control $control was accepted" >&2
    failed=$((failed + 1))
  elif ! grep -Fxq 'FAIL: input-boundary control mutation did not evaluate' "$work/mutation.log"; then
    echo "FAIL: generator failure control $control failed for the wrong reason" >&2
    failed=$((failed + 1))
  fi
  if [[ ! -e "$work/failed-$control" || -e "$work/validated-$control" ]]; then
    echo "FAIL: generator failure control $control did not stop before candidate validation" >&2
    failed=$((failed + 1))
  fi
done
[[ "$failed" -eq 0 ]] || exit 1
echo 'PASS: all four failed mutations stop before their candidate reaches validation.'
for file in \
  k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml \
  k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml \
  k8s/providers/docker/apps/tenant-ascoachingogvaner.yaml \
  k8s/providers/docker/apps/web-app-wedding-app.yaml; do
  mkdir -p "$work/$(dirname "$file")"
  cp "$root/$file" "$work/$file"
done
bash "$root/scripts/scan-rgd-templates.sh" "$work" >"$work/valid.log" 2>&1 || { cat "$work/valid.log" >&2; exit 1; }
instance="$work/k8s/providers/docker/apps/web-app-wedding-app.yaml"
cp "$instance" "$work/original.yaml"
for expression in \
  '.spec.command = ["sh"]' \
  '.spec.runAsUser = 0' \
  '.spec.secretProjections = [{"secretName":"test","namespace":"other","items":[{"key":"one","path":"one"}]}]' \
  '.spec.args = [("$" + "(UNTRUSTED)")]'; do
  cp "$work/original.yaml" "$instance"
  yq -i "$expression" "$instance"
  if bash "$root/scripts/scan-rgd-templates.sh" "$work" >"$work/refused.log" 2>&1; then
    echo 'FAIL: actual publication scanner accepted an unsafe WebApp input' >&2
    exit 1
  fi
  grep -Fq 'committed WebApp has invalid or unknown source inputs' "$work/refused.log" || { cat "$work/refused.log" >&2; exit 1; }
done
cp "$work/original.yaml" "$instance"
printf '\n  runAsUser: 0\n  runAsUser: 65532\n' >>"$instance"
if bash "$root/scripts/scan-rgd-templates.sh" "$work" >"$work/duplicate.log" 2>&1; then
  echo 'FAIL: actual publication scanner accepted duplicate raw YAML fields' >&2
  exit 1
fi
grep -Fq 'committed WebApp has ambiguous raw source' "$work/duplicate.log" || { cat "$work/duplicate.log" >&2; exit 1; }
echo 'PASS: real publication scanner accepts committed inputs and refuses command, root, namespace and interpolation overrides.'
