#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ksail_bin=${KSAIL_BIN:-ksail}
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
cp "$root/k8s/providers/hetzner/infrastructure/cluster-policies/restrict-backstage-retirement.yaml" "$scratch/cluster-policy.yaml"
cat >"$scratch/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [cluster-policy.yaml, helm-release.yaml]
YAML
cat >"$scratch/helm-release.yaml" <<'YAML'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: unrelated
  namespace: fixture
spec:
  interval: 5m
  chartRef:
    kind: HelmChart
    name: unrelated
    namespace: fixture
YAML
if ! output=$("$ksail_bin" --config "$root/ksail.prod.yaml" workload validate "$scratch" --skip-helm-render 2>&1); then
  printf 'FAIL: unrelated release incorrectly needs unavailable retirement state\n%s\n' "$output" >&2
  exit 1
fi
# This control still consumes the real captured chart, despite living elsewhere.
yq -i '.spec.chartRef.name = "backstage-backstage" | .spec.chartRef.namespace = "backstage"' "$scratch/helm-release.yaml"
if output=$("$ksail_bin" --config "$root/ksail.prod.yaml" workload validate "$scratch" --skip-helm-render 2>&1); then
  printf 'FAIL: actual consumer passed without retirement lookup evidence\n' >&2
  exit 1
fi
if ! grep -q 'rule "fence-chart-consumers" errored' <<<"$output"; then
  printf 'FAIL: actual consumer was not stopped by retirement evidence\n%s\n' "$output" >&2
  exit 1
fi
# A release's observed legacy chart identity also makes it a consumer.
yq -i '.spec.chartRef.name = "unrelated" | .spec.chartRef.namespace = "fixture" | .status.helmChart = "backstage/backstage-backstage"' "$scratch/helm-release.yaml"
if output=$("$ksail_bin" --config "$root/ksail.prod.yaml" workload validate "$scratch" --skip-helm-render 2>&1); then
  printf 'FAIL: observed legacy consumer passed without retirement lookup evidence\n' >&2
  exit 1
fi
if ! grep -q 'rule "fence-chart-consumers" errored' <<<"$output"; then
  printf 'FAIL: legacy consumer was not stopped by retirement evidence\n%s\n' "$output" >&2
  exit 1
fi
printf 'PASS: unrelated release needs no retirement evidence; actual consumer still fails closed\n'
