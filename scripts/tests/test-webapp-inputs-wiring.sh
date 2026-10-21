#!/usr/bin/env bash
# Exercise the actual platform publication scanner, not a copied validator call.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
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
echo 'PASS: real publication scanner accepts committed inputs and refuses command, root, namespace and interpolation overrides.'
