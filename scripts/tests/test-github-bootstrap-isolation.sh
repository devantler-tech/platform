#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
work="$(mktemp -d)"
readonly work
trap 'rm -rf "${work}"' EXIT
cd "${root_dir}"

kubectl kustomize k8s/providers/hetzner/apps >"${work}/apps.yaml"
bash scripts/guard-github-bootstrap-isolation.sh "${work}/apps.yaml"
# A parent that checks application health must retain that check. Only the
# asynchronous GitHub API operations are detached from the delivery gate.
kubectl kustomize k8s/clusters/prod >"${work}/cluster.yaml"
yq -e 'select(.kind == "Kustomization" and .metadata.name == "apps") | .spec.wait == true' "${work}/cluster.yaml" >/dev/null

yq 'select(.kind == "Kustomization" and .metadata.name == "github-config")' "${work}/apps.yaml" >"${work}/github.yaml"
for mutation in '.spec.wait = true' '.spec.healthChecks = [{"apiVersion": "repo.github.m.upbound.io/v1alpha1", "kind": "Repository", "name": "pending", "namespace": "github-config"}]'; do
  yq "${mutation}" "${work}/github.yaml" >"${work}/regression.yaml"
  if bash scripts/guard-github-bootstrap-isolation.sh "${work}/regression.yaml" >"${work}/result" 2>&1; then
    printf 'FAIL: a bootstrap-pending resource could gate delivery\n' >&2
    exit 1
  fi
  grep -q 'pending GitHub resources must not gate' "${work}/result"
done

for fixture in empty duplicate malformed; do
  case "${fixture}" in
    empty) printf 'kind: ConfigMap\n' >"${work}/unknown.yaml" ;;
    duplicate) cat "${work}/github.yaml" >"${work}/unknown.yaml"; printf '\n---\n' >>"${work}/unknown.yaml"; cat "${work}/github.yaml" >>"${work}/unknown.yaml" ;;
    malformed) printf 'kind: [\n' >"${work}/unknown.yaml" ;;
  esac
  status=0
  bash scripts/guard-github-bootstrap-isolation.sh "${work}/unknown.yaml" >"${work}/result" 2>&1 || status=$?
  if [[ "${status}" != 2 ]]; then
    printf 'FAIL: incomplete bootstrap census must be UNKNOWN\n' >&2
    exit 1
  fi
done
printf 'PASS: bootstrap regression and incomplete-census controls\n'
