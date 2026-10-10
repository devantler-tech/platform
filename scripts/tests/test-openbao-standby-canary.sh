#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
scratch="$(mktemp -d "${TMPDIR:-/tmp}/openbao-canary.XXXXXX")"
readonly scratch
trap 'rm -rf -- "${scratch}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

checked_postrenderer_changes() {
  jq -en --slurpfile before "$1" --slurpfile after "$2" '
    ($before | length) == 1 and ($after | length) == 1 and
    $after[0].spec.replicas == 3 and
    $after[0].spec.updateStrategy == {type: "OnDelete"} and
    $after[0].spec.template.spec.automountServiceAccountToken == false and
    $before[0].spec.template.metadata.labels["platform.devantler.tech/arc-transport"] == null and
    $after[0].spec.template.metadata.labels["platform.devantler.tech/arc-transport"] == "tls" and
    ($before[0].spec | del(.updateStrategy, .template.spec.automountServiceAccountToken)) ==
    ($after[0].spec | del(.updateStrategy, .template.spec.automountServiceAccountToken,
                         .template.metadata.labels["platform.devantler.tech/arc-transport"]))
  ' >/dev/null
}

resolve_replica_placeholder() {
  OPENBAO_REPLICA_PLACEHOLDER="\${openbao_replicas:=1}" OPENBAO_CANARY_REPLICAS="$2" yq -i '
    (.server.replicas | select(. == strenv(OPENBAO_REPLICA_PLACEHOLDER))) = env(OPENBAO_CANARY_REPLICAS) |
    (.server.ha.replicas | select(. == strenv(OPENBAO_REPLICA_PLACEHOLDER))) = env(OPENBAO_CANARY_REPLICAS)
  ' "$1"
}

kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/controllers" |
  yq ea 'select(.kind == "HelmRelease" and .metadata.name == "openbao")' - >"${scratch}/release.yaml"
readonly release="${scratch}/release.yaml"
chart_version="$(yq -er '.spec.chart.spec.version' "${release}")"
readonly chart_version
[[ "${chart_version}" == '0.29.6' ]] ||
  fail 'the OpenBao chart changed; revalidate the staged rollout before updating this guard'

helm pull openbao --repo https://openbao.github.io/openbao-helm \
  --version "${chart_version}" --destination "${scratch}" >/dev/null
readonly archive="${scratch}/openbao-${chart_version}.tgz"
readonly expected_sha='8079e985bdf608f965ada59c70051693d14dd2454ac16311229f367d0c48c4b9'
if command -v sha256sum >/dev/null 2>&1; then
  actual_sha="$(sha256sum "${archive}" | cut -d ' ' -f 1)"
else
  actual_sha="$(shasum -a 256 "${archive}" | cut -d ' ' -f 1)"
fi
[[ "${actual_sha}" == "${expected_sha}" ]] || fail 'the pinned OpenBao chart checksum changed'

# Exercise this exact server's listener parsing and SIGHUP reload behavior,
# without starting Kubernetes, initializing a vault or accessing credentials.
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)
    native_asset=openbao_2.6.3_linux_amd64.tar.gz
    native_sha=c6463ddd4fdc4214b62a7ffdeaa0fc6df170f7e6b75c6dea2c5e525bdc932ed3
    ;;
  Darwin/arm64)
    native_asset=openbao_2.6.3_darwin_arm64.tar.gz
    native_sha=ed17491ebc6415d075b7b2b2c0ef6b5908b84b161b8e5a50c7da303cd8d15df4
    ;;
  *) fail 'the native TLS regression needs a reviewed server archive for this host' ;;
esac
curl --proto '=https' --proto-redir '=https' --location --tlsv1.2 --fail --silent --show-error --retry 2 --max-time 120 \
  "https://github.com/openbao/openbao/releases/download/v2.6.3/${native_asset}" \
  --output "${scratch}/native.tar.gz"
if command -v sha256sum >/dev/null 2>&1; then
  actual_native_sha="$(sha256sum "${scratch}/native.tar.gz" | cut -d ' ' -f 1)"
else
  actual_native_sha="$(shasum -a 256 "${scratch}/native.tar.gz" | cut -d ' ' -f 1)"
fi
[[ "${actual_native_sha}" == "${native_sha}" ]] || fail 'the pinned native OpenBao archive checksum changed'
mkdir "${scratch}/native"
tar -xzf "${scratch}/native.tar.gz" -C "${scratch}/native"
OPENBAO_TLS_TEST_BINARY="${scratch}/native/bao" go test \
  "${root_dir}/scripts/tests/openbao-transport-runtime" -count=1

replicas="$(yq -er '.data.openbao_replicas' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml")"
readonly replicas
[[ "${replicas}" == '3' ]] || fail 'the OpenBao canary requires revalidation when replica count changes'
yq '.spec.values' "${release}" >"${scratch}/values.yaml"
# Substitute only the configured Flux placeholder; literal effective values must
# remain visible to the rendered replica-count guard rather than being overridden.
resolve_replica_placeholder "${scratch}/values.yaml" "${replicas}"
helm template openbao "${archive}" --namespace openbao --values "${scratch}/values.yaml" \
  >"${scratch}/rendered.yaml"

# Apply the actual Flux post-renderer patches, not just their declared values.
yq -o=json '.spec.postRenderers' "${release}" | jq -e '
  length == 1 and
  (.[0] | keys) == ["kustomize"] and
  (.[0].kustomize | keys) == ["patches"] and
  (.[0].kustomize.patches | length) == 1 and
  (.[0].kustomize.patches[0] | keys) == ["patch", "target"] and
  .[0].kustomize.patches[0].target == {
    "group": "apps", "version": "v1", "kind": "StatefulSet",
    "name": "openbao", "namespace": "openbao"
  }
' >/dev/null || fail 'production OpenBao must carry exactly one reviewed StatefulSet post-renderer patch'
yq '.spec.postRenderers[0].kustomize.patches // [] |
  {"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
   "resources": ["rendered.yaml"], "patches": .}' "${release}" >"${scratch}/kustomization.yaml"
kubectl kustomize "${scratch}" |
  yq ea 'select(.kind == "StatefulSet" and .metadata.name == "openbao")' - >"${scratch}/statefulset.yaml"
readonly statefulset="${scratch}/statefulset.yaml"

# A partitioned rolling update recreates a lost lower ordinal from the stored
# current revision, which admission refuses in production (#4391). OnDelete
# recreates it from the newest revision and replaces no running server.
yq -e '.spec.updateStrategy.type == "OnDelete" and
  (.spec.updateStrategy | length) == 1 and
  .spec.replicas == 3' "${statefulset}" >/dev/null ||
  fail 'production OpenBao must recreate a lost server from the newest revision and replace no running one'
yq -e '.spec.template.spec.containers[] | select(.name == "openbao") |
  .image == "quay.io/openbao/openbao:2.6.3"' "${statefulset}" >/dev/null ||
  fail 'a recreated server must use the released standby OIDC repair'

yq -e '.spec.template.spec.automountServiceAccountToken == false' "${statefulset}" >/dev/null ||
  fail 'the certificate reload helper must not receive an injected API token'

yq ea -o=json -I=0 'select(.kind == "StatefulSet" and .metadata.name == "openbao")' \
  "${scratch}/rendered.yaml" >"${scratch}/before.json"
yq -o=json -I=0 '.' "${statefulset}" >"${scratch}/after.json"
checked_postrenderer_changes "${scratch}/before.json" "${scratch}/after.json" ||
  fail 'the canary post-renderer must change only token injection and the exact TLS identity label'

# Exercise rejected mutations against the actual chart and Flux-rendered Pod.
# Allowing one retained identity label must not permit other metadata or spec edits.
for mutation in \
  'del(.spec.template.metadata.labels["platform.devantler.tech/arc-transport"])' \
  '.spec.template.metadata.labels["platform.devantler.tech/arc-transport"] = "other"' \
  '.spec.template.metadata.labels["unreviewed-label"] = "extra"' \
  '.spec.template.metadata.annotations["unreviewed-annotation"] = "extra"' \
  '.spec.template.metadata.labels["app.kubernetes.io/name"] = "other"' \
  '.spec.template.spec.serviceAccountName = "other"' \
  '.spec.template.spec.automountServiceAccountToken = true' \
  '.spec.updateStrategy = {"type": "RollingUpdate"}' \
  '.spec.updateStrategy = {"type": "RollingUpdate", "rollingUpdate": {"partition": 2}}' \
  '.spec.updateStrategy.rollingUpdate = {"partition": 0}' \
  '.spec.replicas = 1'; do
  jq "$mutation" "${scratch}/after.json" >"${scratch}/mutated.json"
  if checked_postrenderer_changes "${scratch}/before.json" "${scratch}/mutated.json"; then
    fail 'the canary post-renderer accepted an unreviewed identity, metadata or Pod-spec mutation'
  fi
done

# Validate the actual pinned chart and post-rendered security boundary. An
# unsupported Helm value must not look like it removed the sidecar's token.
yq -o=json "${statefulset}" | jq -e '.spec.template.spec.shareProcessNamespace == true and
  ((.spec.template.spec.hostPID // false) == false) and
  ((.spec.template.spec.hostNetwork // false) == false)' >/dev/null ||
  fail 'native certificate reload must stay inside the trusted server Pod'
yq -o=json "${statefulset}" | jq -e '.spec.template.spec.containers[] | select(.name == "arc-tls-reload") |
  .image == "quay.io/openbao/openbao:2.6.3" and
  ((.volumeMounts // []) | length == 0) and
  .securityContext.runAsUser == 100 and
  .securityContext.allowPrivilegeEscalation == false and
  .securityContext.readOnlyRootFilesystem == true' >/dev/null ||
  fail 'certificate reload must receive neither credential mounts nor additional privilege'
yq -o=json "${statefulset}" | jq -e '.spec.template.spec.containers[] | select(.name == "openbao") |
  any(.volumeMounts[]; .name == "kube-api-access" and
    .mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" and .readOnly == true)' >/dev/null ||
  fail 'the native server must retain its own explicit Kubernetes API identity'

# Local/default installations keep the chart's deliberate OnDelete behavior.
yq '.spec.values' "${root_dir}/k8s/bases/infrastructure/controllers/openbao/helm-release.yaml" >"${scratch}/base-values.yaml"
# Resolve the base's ${openbao_replicas:=1} Flux default for Helm's integer schema.
resolve_replica_placeholder "${scratch}/base-values.yaml" 1
helm template openbao "${archive}" --namespace openbao --values "${scratch}/base-values.yaml" |
  yq ea -e 'select(.kind == "StatefulSet" and .metadata.name == "openbao") |
    .spec.updateStrategy.type == "OnDelete"' - >/dev/null ||
  fail 'the staged production rollout must not enable local/default automatic replacement'

grep -Fq "'scripts/tests/test-openbao-standby-canary.sh'" "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'the OpenBao canary regression must trigger CI'
grep -Fq 'bash scripts/tests/test-openbao-standby-canary.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must execute the OpenBao canary regression'

printf 'PASS: production OpenBao recreates a lost server at 2.6.3 and replaces no running one\n'
