#!/usr/bin/env bash
# Bind offline Helm capabilities to the admitted controller image and CRD pair.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ "$#" -eq 0 ]; then
  [[ "$(yq -er '.spec.ref.tag' "$root/k8s/bases/infrastructure/controllers/flux-operator/oci-repository.yaml")" == 0.62.1 ]] || {
    echo 'guard-controller-helm-profile: the operator composition must be re-audited' >&2
    exit 2
  }
  set -- "$root/k8s/providers/docker/infrastructure/controllers/flux-instance/flux-instance.yaml" \
    "$root/k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml"
fi
fail() {
  echo "guard-controller-helm-profile: $*" >&2
  exit 2
}
for instance in "$@"; do
  data="$(yq -o=json '.' "$instance")" || fail "cannot parse $instance"
  jq -e '
    .apiVersion == "fluxcd.controlplane.io/v1" and .kind == "FluxInstance" and
    .metadata.name == "flux" and .metadata.namespace == "flux-system" and
    .spec.distribution == {version:"2.9.6",registry:"ghcr.io/fluxcd"} and
    (.spec.components | sort) == ["helm-controller","kustomize-controller","notification-controller","source-controller"] and
    .spec.cluster.multitenant == false and
    (.spec.cluster.objectIdentityRBAC // false) == false and
    (.spec.cluster.objectLevelWorkloadIdentity // false) == false and
    .spec.spiffe == null and
    ([.spec.kustomize.patches[] | select(.target.kind == "Deployment" and .target.name == "helm-controller")]
      | length == 1 and .[0].target == {kind:"Deployment",name:"helm-controller"}) and
    ([.spec.kustomize.patches[] | select(.target.kind == "CustomResourceDefinition")]
      | length == 1 and .[0].target == {kind:"CustomResourceDefinition",name:"helmreleases.helm.toolkit.fluxcd.io"})
  ' <<<"$data" >/dev/null || fail "$instance is not the audited reversible Flux/controller profile"
  image_ops="$(yq -o=json '.spec.kustomize.patches[] | select(.target.kind == "Deployment" and .target.name == "helm-controller") | .patch | from_yaml' "$instance")" || fail 'cannot parse controller image operations'
  jq -e '
    .[-3:] == [
      {op:"test",path:"/spec/template/spec/containers/0/name",value:"manager"},
      {op:"test",path:"/spec/template/spec/containers/0/image",value:"fluxcd/helm-controller:v1.6.5"},
      {op:"replace",path:"/spec/template/spec/containers/0/image",value:"ghcr.io/fluxcd/helm-controller:v1.7.1@sha256:54526cc6ed1202d71d44c310676448065664f3255ad671d9a69ac26f904e617a"}]
  ' <<<"$image_ops" >/dev/null || fail 'the guarded signed controller image override changed'
  crd_ops="$(yq -o=json '.spec.kustomize.patches[] | select(.target.kind == "CustomResourceDefinition") | .patch | from_yaml' "$instance")" || fail 'cannot parse generated CRD operations'
  actual="$(jq -cS . <<<"$crd_ops" | shasum -a 256 | cut -d ' ' -f 1)"
  # Official v0.62.1 `patch instance -v 10 -c helm-controller` output plus the
  # GVK/v2 input tests. The native composition test independently binds the
  # resulting complete spec to upstream helm-controller v1.7.1.
  [[ "$actual" == fddcb81fffe4ad06b2f9088c1c2c0986f352d5e8e4f89bfe998b05f4b65f3f68 ]] || fail 'the generated controller CRD patch or its input guards changed'
done
