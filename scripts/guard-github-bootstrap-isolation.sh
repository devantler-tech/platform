#!/usr/bin/env bash
set -euo pipefail

manifest="${1:-k8s/bases/apps/github-config/flux-kustomization.yaml}"
if [[ ! -r "${manifest}" ]]; then
  printf 'UNKNOWN: cannot read GitHub bootstrap configuration\n' >&2
  exit 2
fi
if ! config="$(yq eval-all -o=json -I=0 '[select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and .metadata.name == "github-config")]' "${manifest}")"; then
  printf 'UNKNOWN: cannot parse GitHub bootstrap configuration\n' >&2
  exit 2
fi
if ! jq -e 'length == 1' <<<"${config}" >/dev/null; then
  printf 'UNKNOWN: expected exactly one GitHub Kustomization\n' >&2
  exit 2
fi
if ! jq -e '.[0].spec | .wait == false and ((.healthChecks // []) | length) == 0' <<<"${config}" >/dev/null; then
  printf 'FAIL: pending GitHub resources must not gate application delivery; disable wait and omit explicit health checks\n' >&2
  exit 1
fi
printf 'PASS: GitHub resource readiness is isolated from application delivery\n'
