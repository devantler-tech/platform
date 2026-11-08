#!/usr/bin/env bash
# Prove Substrate's bucket-init Job ships as a Helm hook, not as an ordinary release object.
#
# The chart renders `rustfs-bucket-init` as a plain Job whose pod template carries the RustFS
# keys and the aws-cli image. A Job's template is immutable, so once the finished Job exists,
# any upgrade that changes either fails server-side apply, and keeps failing (#5903 failed its
# prod deploy twice on it). The release's post-renderer makes the Job a post-install and
# post-upgrade hook that Helm deletes once it succeeds. This renders the pinned chart with the
# release's values, applies the release's post-renderers the way Flux does, and asks the
# controller's own Helm SDK where it sorts the Job.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
base="${root}/k8s/bases/infrastructure/substrate"
work="$(mktemp -d)"
completed=0
# Remove the work directory on exit, and fail any exit that did not reach the final PASS, so an
# abort the shell does not report (bash 3.2 can mask `set -u` here) never reads as a pass.
cleanup() {
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: substrate bucket-init hook: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: substrate bucket-init hook: %s\n' "$1" >&2
  exit 1
}
for tool in kubectl jq yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done
[[ -n "${CONTROLLER_HELM:-}" && -x "${CONTROLLER_HELM}" ]] || fail 'the audited controller SDK renderer is required (CONTROLLER_HELM)'
[[ "$("${CONTROLLER_HELM}" version --template '{{ printf "%#v" . }}')" == 'version.BuildInfo{Version:"v4.2", GitCommit:"", GitTreeState:"", GoVersion:"go1.26.8", KubeClientVersion:"v1.36"}' ]] ||
  fail 'controller SDK renderer metadata does not match Flux 2.9.6'

job='rustfs-bucket-init'
namespace='ate-system'
hook='post-install,post-upgrade'
policy='before-hook-creation,hook-succeeded'

# The release and its chart source, as Flux applies them.
kubectl kustomize "${base}" >"${work}/base.yaml"
yq -o=json 'select(.kind == "HelmRelease" and .metadata.name == "substrate")' "${work}/base.yaml" >"${work}/release.json"
yq -o=json 'select(.kind == "OCIRepository" and .metadata.name == "substrate")' "${work}/base.yaml" >"${work}/source.json"
[[ -s "${work}/release.json" && -s "${work}/source.json" ]] || fail 'the substrate HelmRelease or its OCIRepository is missing'
jq -e '.spec.postRenderStrategy == "nohooks"' "${work}/release.json" >/dev/null ||
  fail 'the release must post-render only its manifest stream, which this test and the guard model'
jq -e '[.spec.valuesFrom[].targetPath] | sort == ["rustfs.accessKey", "rustfs.secretKey"]' "${work}/release.json" >/dev/null ||
  fail 'the release takes values from a Secret this test does not stand in for'

# Pull the pinned chart anonymously; a workstation's credential helper is not needed for a public chart.
mkdir -p "${work}/docker" "${work}/chart"
url="$(jq -r '.spec.url' "${work}/source.json")"
tag="$(jq -r '.spec.ref.tag' "${work}/source.json")"
export DOCKER_CONFIG="${work}/docker" HELM_REGISTRY_CONFIG="${work}/docker/config.json"
export HELM_CACHE_HOME="${work}/helm/cache" HELM_CONFIG_HOME="${work}/helm/config" HELM_DATA_HOME="${work}/helm/data" HELM_PLUGINS="${work}/helm/plugins"
"${CONTROLLER_HELM}" pull "${url}" --version "${tag}" --destination "${work}/chart" >/dev/null 2>"${work}/pull.err" ||
  fail "cannot pull ${url}:${tag}: $(head -c 300 "${work}/pull.err")"
chart="$(find "${work}/chart" -name '*.tgz' -print -quit)"
[[ -n "${chart}" ]] || fail 'the pulled chart archive is missing'

# The Secret-held keys render as placeholders, as in the post-renderer guard.
jq '.spec.values | .rustfs.accessKey = "placeholder" | .rustfs.secretKey = "placeholder"' \
  "${work}/release.json" >"${work}/values.json"
"${CONTROLLER_HELM}" template substrate "${chart}" --namespace "${namespace}" --is-upgrade \
  --values "${work}/values.json" >"${work}/chart.yaml"

# Index a multi-document manifest by kind/namespace/name, so two renders compare as one object.
normalized() { # <manifest file>
  yq ea -o=json '[. | select(tag == "!!map")]' "$1" |
    jq -S 'map({key: "\(.kind)/\(.metadata.namespace // "")/\(.metadata.name)", value: .}) | from_entries'
}

# Premises. The chart must still render the Job as an ordinary object, or this patch should go;
# and with no hooks of its own, Helm's combined stream below is exactly what Flux's nohooks
# strategy post-renders, so the CLI's sort is the controller's sort.
normalized "${work}/chart.yaml" >"${work}/chart.json"
key="Job/${namespace}/${job}"
jq -e --arg key "${key}" '.[$key] != null and (.[$key].metadata.annotations // {} | has("helm.sh/hook") | not)' \
  "${work}/chart.json" >/dev/null || fail 'the chart now renders the Job as a hook itself; retire the post-renderer patch'
jq -e '[.[] | select(.metadata.annotations["helm.sh/hook"] != null)] | length == 0' "${work}/chart.json" >/dev/null ||
  fail 'the chart now renders hooks of its own; the nohooks stream no longer equals the combined one'

# Apply the release's post-renderers in order, the way Flux builds them, and write the
# normalized result to <out>.json. The baseline drops only the Job patch, to prove that patch
# changes nothing but the Job's hook annotations.
post_render() { # <out> <jq filter over one renderer's patches>
  local dir="${work}/render-$1" count index
  mkdir -p "${dir}"
  cp "${work}/chart.yaml" "${dir}/resources.yaml"
  count="$(jq '.spec.postRenderers | length' "${work}/release.json")"
  for ((index = 0; index < count; index++)); do
    jq --argjson index "${index}" '{apiVersion: "kustomize.config.k8s.io/v1beta1",
      kind: "Kustomization", resources: ["resources.yaml"]} + .spec.postRenderers[$index].kustomize' \
      "${work}/release.json" | jq "$2" >"${dir}/kustomization.yaml"
    kubectl kustomize "${dir}" >"${dir}/next.yaml"
    mv "${dir}/next.yaml" "${dir}/resources.yaml"
  done
  normalized "${dir}/resources.yaml" >"${work}/$1.json"
}
post_render after '.'
post_render baseline '.patches |= map(select(.target.kind != "Job"))'

jq -e --arg key "${key}" --arg hook "${hook}" --arg policy "${policy}" '
  .[$key].metadata.annotations["helm.sh/hook"] == $hook and
  .[$key].metadata.annotations["helm.sh/hook-delete-policy"] == $policy
' "${work}/after.json" >/dev/null ||
  fail "the Job must be a ${hook} hook that Helm deletes on success and replaces before the next run"
jq -e --arg key "${key}" --slurpfile baseline "${work}/baseline.json" '
  del(.[$key].metadata.annotations["helm.sh/hook"], .[$key].metadata.annotations["helm.sh/hook-delete-policy"])
  | if (.[$key].metadata.annotations // {}) == {} then del(.[$key].metadata.annotations) else . end
  | . == $baseline[0]
' "${work}/after.json" >/dev/null || fail 'the Job patch changed something other than its two hook annotations'

# The name must not move: the chart's NetworkPolicy admits the Job's pods to RustFS by it.
jq -e --arg job "${job}" '
  [.[] | select(.kind == "NetworkPolicy") | (.spec.ingress // [])[] | (.from // [])[]
    | .podSelector.matchLabels["batch.kubernetes.io/job-name"] // empty]
  | index($job) != null
' "${work}/after.json" >/dev/null || fail 'the chart NetworkPolicy no longer admits the Job by its name'

# Ask the controller's Helm where it sorts the Job. Helm 4.2.4 sorts hooks after
# post-rendering under every strategy, so the annotation alone decides it.
plugin="${HELM_PLUGINS}/substrate-hook-proof"
mkdir -p "${plugin}" "${work}/plugin-run"
cat >"${plugin}/plugin.yaml" <<'YAML'
apiVersion: v1
type: postrenderer/v1
name: substrate-hook-proof
version: 0.1.0
runtime: subprocess
runtimeConfig:
  platformCommand:
    - command: ${HELM_PLUGIN_DIR}/run.sh
YAML
cat >"${plugin}/run.sh" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
dir="${SUBSTRATE_HOOK_RUN:?}"
cat >"${dir}/resources.yaml"
count="$(jq '.spec.postRenderers | length' "${SUBSTRATE_HOOK_RELEASE:?}")"
for ((index = 0; index < count; index++)); do
  jq --argjson index "${index}" '{apiVersion: "kustomize.config.k8s.io/v1beta1",
    kind: "Kustomization", resources: ["resources.yaml"]} + .spec.postRenderers[$index].kustomize' \
    "${SUBSTRATE_HOOK_RELEASE}" >"${dir}/kustomization.yaml"
  kubectl kustomize "${dir}" >"${dir}/next.yaml"
  mv "${dir}/next.yaml" "${dir}/resources.yaml"
done
cat "${dir}/resources.yaml"
BASH
chmod +x "${plugin}/run.sh"
SUBSTRATE_HOOK_RUN="${work}/plugin-run" SUBSTRATE_HOOK_RELEASE="${work}/release.json" \
  "${CONTROLLER_HELM}" install substrate "${chart}" --namespace "${namespace}" --dry-run=client \
  --values "${work}/values.json" --post-renderer substrate-hook-proof >"${work}/dry-run.txt" 2>"${work}/dry-run.err" ||
  fail "the controller Helm cannot install the post-rendered chart: $(head -c 300 "${work}/dry-run.err")"
awk '/^HOOKS:$/ {s = "hooks"; next} /^MANIFEST:$/ {s = "manifest"; next} /^NOTES:$/ {s = ""}
  s != "" {print > (dir "/" s ".yaml")}' dir="${work}" "${work}/dry-run.txt"
[[ -s "${work}/hooks.yaml" && -s "${work}/manifest.yaml" ]] || fail 'the dry run printed no HOOKS or MANIFEST section'
hooks="$(yq ea -o=json '[. | select(tag == "!!map") | "\(.kind)/\(.metadata.name)"]' "${work}/hooks.yaml" | jq -c 'sort')"
[[ "${hooks}" == "[\"Job/${job}\"]" ]] || fail "Helm must sort exactly the Job as a hook, got ${hooks}"
yq ea -o=json '[. | select(tag == "!!map") | "\(.kind)/\(.metadata.name)"]' "${work}/manifest.yaml" |
  jq -e --arg job "Job/${job}" 'index($job) == null and length > 0' >/dev/null ||
  fail 'the Job must not remain an ordinary release object'

grep -Fq 'bash scripts/tests/test-substrate-bucket-init-hook.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI must run this test'
completed=1
echo 'substrate bucket-init hook: PASS'
