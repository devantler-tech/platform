#!/usr/bin/env bash
# Static authorization/path contract. Publication and live DNS need separate proof.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
kubectl kustomize "$repo_root/k8s/providers/hetzner/apps/khoist-documentation" >"$work/resources.yaml"
yq -o=json -I=0 '.' "$work/resources.yaml" | jq -s '.' >"$work/resources.json"
validate() {
  jq -e '
    length==5 and all(.[]; .metadata.name=="khoist-documentation" and .metadata.namespace=="data-product-controller") and
    ([.[].kind]|sort)==["Kustomization","OCIRepository","Role","RoleBinding","ServiceAccount"] and
    (map(select(.kind=="ServiceAccount"))[0] | .automountServiceAccountToken==false) and
    (map(select(.kind=="Role"))[0] | .rules==[{apiGroups:["externaldns.k8s.io"],resources:["dnsendpoints"],verbs:["get","list","watch","create","update","patch","delete"]}]) and
    (map(select(.kind=="RoleBinding"))[0] |
      .roleRef=={apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"khoist-documentation"} and
      .subjects==[{kind:"ServiceAccount",name:"khoist-documentation",namespace:"data-product-controller"}]) and
    (map(select(.kind=="OCIRepository"))[0] |
      .spec.url=="oci://ghcr.io/devantler-tech/data-product-controller/manifests" and
      (.spec.ref|keys)==["digest"] and (.spec.ref.digest|test("^sha256:[0-9a-f]{64}$")) and
      .spec.secretRef.name=="ghcr-auth" and .spec.verify.provider=="cosign" and
      (.spec.verify.matchOIDCIdentity|length)==1) and
    (map(select(.kind=="Kustomization"))[0] |
      .spec.path=="./documentation" and .spec.targetNamespace=="data-product-controller" and
      .spec.serviceAccountName=="khoist-documentation" and
      .spec.sourceRef=={kind:"OCIRepository",name:"khoist-documentation"} and
      .spec.dependsOn==[{name:"infrastructure",namespace:"flux-system"}] and
      .spec.prune==true and .spec.wait==true and .spec.force==false)
  ' "$1" >/dev/null
}
validate "$work/resources.json"
# Check the actual parent overlays as well: a shared transform must not broaden
# the role, and a local cluster must not consume this production DNS tenant.
kubectl kustomize "$repo_root/k8s/providers/hetzner/apps" >"$work/production.yaml"
yq -o=json -I=0 '.' "$work/production.yaml" | jq -s '[.[]|select(.metadata.name=="khoist-documentation")]' >"$work/production.json"
validate "$work/production.json"
kubectl kustomize "$repo_root/k8s/providers/docker/apps" >"$work/local.yaml"
yq -o=json -I=0 '.' "$work/local.yaml" | jq -es 'all(.[]; .metadata.name!="khoist-documentation")' >/dev/null
# A changed trust boundary must fail this guard, not merely change its output.
for mutation in \
  'map(if .kind=="Role" then .rules[0].resources=["secrets"] else . end)' \
  'map(if .kind=="RoleBinding" then .roleRef.kind="ClusterRole" else . end)' \
  'map(if .kind=="Kustomization" then .spec.path="." else . end)' \
  'map(if .kind=="ServiceAccount" then .metadata.namespace="another-tenant" else . end)'; do
  jq "$mutation" "$work/resources.json" >"$work/widened.json"
  if validate "$work/widened.json"; then
    echo 'documentation tenant guard accepted a widened authorization/path boundary' >&2
    exit 1
  fi
done
echo 'documentation tenant uses only its DNS role and product documentation subdirectory'
