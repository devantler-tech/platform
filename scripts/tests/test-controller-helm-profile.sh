#!/usr/bin/env bash
# Compose the committed instances with the released operator, not a patch model.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
operator="${FLUX_OPERATOR_BIN:-flux-operator}"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
fail() {
  echo "FAIL: $*" >&2
  exit 1
}
[[ "$(yq -er '.spec.ref.tag' "$root/k8s/bases/infrastructure/controllers/flux-operator/oci-repository.yaml")" == 0.62.1 ]] || fail 'native composition must use the admitted operator version'
for provider in docker hetzner; do
  instance="$root/k8s/providers/$provider/infrastructure/controllers/flux-instance/flux-instance.yaml"
  [[ "$(yq -er '.spec.distribution.version' "$instance")" == 2.9.6 ]] || fail 'the reversible distribution must stay on exact 2.9.6'
  bash "$root/scripts/guard-controller-helm-profile.sh" "$instance"
done
[[ "$("$operator" version --client)" == 'client: 0.62.1' ]] || fail 'released flux-operator 0.62.1 is required'
artifact='oci://ghcr.io/controlplaneio-fluxcd/flux-operator-manifests@sha256:033a75c33b911d534e10c58cbcc60dda75027c99054fcd414339c9fd5019f7c3'
for provider in docker hetzner; do
  instance="$root/k8s/providers/$provider/infrastructure/controllers/flux-instance/flux-instance.yaml"
  "$operator" build instance --distribution-artifact "$artifact" -f "$instance" >"$work/$provider.yaml"
  yq ea -o=json -I=0 'select(.kind != null)' "$work/$provider.yaml" | jq -s . >"$work/$provider.json"
  jq -e --arg provider "$provider" '
    length == 32 and
    ([.[] | select(.kind == "Deployment") | {name:.metadata.name,image:.spec.template.spec.containers[0].image}] | sort_by(.name)) ==
      [{name:"helm-controller",image:"ghcr.io/fluxcd/helm-controller:v1.7.1@sha256:54526cc6ed1202d71d44c310676448065664f3255ad671d9a69ac26f904e617a"},
       {name:"kustomize-controller",image:"ghcr.io/fluxcd/kustomize-controller:v1.9.6"},
       {name:"notification-controller",image:"ghcr.io/fluxcd/notification-controller:v1.9.4"},
       {name:"source-controller",image:"ghcr.io/fluxcd/source-controller:v1.9.6"}] and
    all(.[] | select(.kind == "Deployment");
      .spec.replicas == (if $provider == "hetzner" and .metadata.name != "source-controller" then 2 else 1 end)) and
    all(.[] | select(.kind == "Deployment" and .metadata.name == "helm-controller");
      (.spec.template.spec.containers[0].args | index("--feature-gates=ObjectLevelWorkloadIdentity=false") != null) and
      (if $provider == "hetzner" then .spec.minReadySeconds == 30 and
        .spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey == "kubernetes.io/hostname" else true end)) and
    ([.[] | select(.kind == "ClusterRole" or .kind == "Role") | .rules[]?.resources[]? | select(. == "serviceaccounts/token")] | length == 0)
  ' "$work/$provider.json" >/dev/null || fail "$provider native image, HA or identity boundary changed"
  actual="$(jq -cS '.[] | select(.kind == "CustomResourceDefinition" and .metadata.name == "helmreleases.helm.toolkit.fluxcd.io") | .spec' "$work/$provider.json" | shasum -a 256 | cut -d ' ' -f 1)"
  [[ "$actual" == 6adcc483cf20bf1e8d75963504772c5b00917def5aece1e8aaefdc877bb25fcb ]] || fail "$provider CRD differs from the audited upstream v1.7.1 spec"
  # 1.7 retires the ObjectLevelWorkloadIdentity switch. Keep its TokenRequest
  # grants absent and reject new object-identity consumers until re-audited.
  for layer in infrastructure/controllers infrastructure apps; do
    kubectl kustomize "$root/k8s/providers/$provider/$layer" >"$work/layer-${layer//\//-}.yaml"
  done
  yq ea -o=json -I=0 'select(.kind != null)' "$work"/layer-*.yaml | jq -s . >"$work/consumers.json"
  jq -e '
    ([.[] | select(.kind == "HelmRelease")] | length > 0) and
    all(.[] | select(.kind == "HelmRelease"); .spec.serviceAccountName == null and .spec.kubeConfig == null) and
    all(.[] | select(.kind == "ServiceAccount");
      (.metadata.annotations // {} | keys | map(select(test("identity|iam|azure|gcp|aws|role-arn|workload"))) | length == 0))
  ' "$work/consumers.json" >/dev/null || fail "$provider gained an unaudited identity consumer"
  for mutation in \
    '.spec.distribution.version = "2.10.0"' \
    '.spec.distribution.version = "2.9.x"' \
    '.spec.kustomize.patches |= map(select(.target.kind != "CustomResourceDefinition"))' \
    '.spec.kustomize.patches += [.spec.kustomize.patches[] | select(.target.kind == "Deployment" and .target.name == "helm-controller")]' \
    '(.spec.kustomize.patches[] | select(.target.kind == "Deployment" and .target.name == "helm-controller").patch) |= sub("v1.7.1@sha256:", "v1.7.2@sha256:")'; do
    yq "$mutation" "$instance" >"$work/mutated.yaml"
    if bash "$root/scripts/guard-controller-helm-profile.sh" "$work/mutated.yaml" >"$work/mutation.log" 2>&1; then fail 'unaudited controller profile passed'; fi
  done
done
# Execute the native JSON-patch refusal itself, beyond the offline admission
# guard: an operator that ignored test operations would make both cases pass.
instance="$root/k8s/providers/docker/infrastructure/controllers/flux-instance/flux-instance.yaml"
for field in image crd; do
  if [ "$field" = image ]; then
    mutation='(.spec.kustomize.patches[] | select(.target.kind == "Deployment" and .target.name == "helm-controller").patch) |= sub("value: manager", "value: wrong-manager")'
    refusal='testing value /spec/template/spec/containers/0/name failed'
  else
    mutation='(.spec.kustomize.patches[] | select(.target.kind == "CustomResourceDefinition").patch) |= sub("value: v2", "value: wrong-version")'
    refusal='testing value /spec/versions/0/name failed'
  fi
  yq "$mutation" "$instance" >"$work/native-refusal.yaml"
  if "$operator" build instance --distribution-artifact "$artifact" -f "$work/native-refusal.yaml" >"$work/native-refusal.out" 2>"$work/native-refusal.log"; then fail 'native operator ignored an input test'; fi
  grep -Fq "$refusal" "$work/native-refusal.log" || fail 'native refusal was not the expected input guard failure'
done
node - "$root" <<'NODE'
const fs = require('node:fs');
const root = process.argv[2];
const policy = JSON.parse(fs.readFileSync(`${root}/.github/renovate.json`, 'utf8'));
for (const [name, version, allowed] of [
  ['fluxcd/flux2', '2.9.6', '/^v?2\\.9\\.6$/'],
  ['ghcr.io/fluxcd/helm-controller', 'v1.7.1', '/^v?1\\.7\\.1$/'],
]) {
  const managers = policy.customManagers.filter(x => x.depNameTemplate === name);
  const rules = policy.packageRules.filter(x => (x.matchPackageNames || []).includes(name));
  if (managers.length !== 1 || rules.length !== 1 || rules[0].allowedVersions !== allowed || rules[0].enabled === false || rules[0].automerge !== false) throw Error(`unbound controller profile update policy: ${name}`);
  const manager = managers[0];
  for (const provider of ['docker', 'hetzner']) {
    const path = `k8s/providers/${provider}/infrastructure/controllers/flux-instance/flux-instance.yaml`;
    if (!manager.managerFilePatterns.some(p => new RegExp(p.slice(1, -1)).test(path))) throw Error(`missing ${provider} profile discovery`);
    const matches = [...fs.readFileSync(`${root}/${path}`, 'utf8').matchAll(new RegExp(manager.matchStrings[0], 'g'))];
    if (matches.length !== 1 || matches[0].groups.currentValue !== version) throw Error(`wrong ${provider} profile selector`);
    if (name.endsWith('helm-controller') && matches[0].groups.currentDigest !== 'sha256:54526cc6ed1202d71d44c310676448065664f3255ad671d9a69ac26f904e617a') throw Error('image update discovery lost its digest');
  }
}
NODE
echo 'PASS: released operator composes the pinned controller image/CRD pair, replicas and identity boundary; unaudited profiles are refused'
