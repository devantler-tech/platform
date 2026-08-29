#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly production_infrastructure="${root_dir}/k8s/providers/hetzner/infrastructure"
readonly production_controllers="${production_infrastructure}/controllers"
readonly retired_controller="${production_controllers}/tofu-controller"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 ||
  fail 'kubectl is required to render the production infrastructure overlay'
command -v yq >/dev/null 2>&1 ||
  fail 'yq v4 is required to inspect the rendered retirement contract'

if [[ -e "${retired_controller}" ]]; then
  fail 'the retired tofu-controller deployment root remains independently renderable'
fi

rendered="$(kubectl kustomize "${production_controllers}")" ||
  fail 'the production controllers overlay must render successfully'

printf '%s\n' "${rendered}" |
  yq ea -e '
    [select(
      .metadata.name == "tofu-controller" or
      .metadata.name == "tofu-cluster-reconciler-role" or
      .metadata.name == "tofu-manager-role" or
      .metadata.name == "tofu-cluster-reconciler" or
      .metadata.name == "tofu-manager-rolebinding"
    )] |
    length == 0
  ' - >/dev/null ||
  fail 'the production controllers render must not contain tofu-controller resources'

printf '%s\n' "${rendered}" |
  yq ea -e '
    [select(
      .apiVersion == "apiextensions.k8s.io/v1" and
      .kind == "CustomResourceDefinition" and
      .metadata.name == "terraforms.infra.contrib.fluxcd.io"
    )] |
    length == 1
  ' - >/dev/null ||
  fail 'the first retirement stage must inventory the empty Terraform CRD before pruning it'

printf 'PASS: the production inventory cannot deploy the retired tofu-controller\n'
