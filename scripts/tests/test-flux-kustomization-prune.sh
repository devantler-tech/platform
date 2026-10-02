#!/usr/bin/env bash
# Every Flux Kustomization the platform renders must prune, so GitOps stays
# authoritative: Flux defaults spec.prune to false, so an omitted field silently
# opts a whole tree out of deletion (#3373).
#
# The one tree that needs protection, unifi, keeps it at the resource layer: its
# patches strip the Delete management policy from every UniFi managed resource,
# so pruning a managed resource never deletes the live network object, and a
# resource annotated observe-only for adoption is narrowed to Observe. The
# patches are exercised by rendering, not by reading its selector, because a target
# that matches nothing renders and applies clean while removing the protection.
#
# Set UNIFI_SOURCE_DIR to a devantler-tech/unifi checkout to also render the
# real source through the patch.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly unifi_kustomization="${root_dir}/k8s/providers/hetzner/apps/unifi/flux-kustomization.yaml"
readonly unifi_role="${root_dir}/k8s/providers/hetzner/apps/unifi/role.yaml"
readonly unifi_policy="${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-unifi-management-policies.yaml"
readonly tenant_rgd="${root_dir}/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"
readonly retained_policies='Observe,Create,Update,LateInitialize'
readonly observe_annotation="platform.devantler.tech/unifi-management"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the overlays'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect rendered manifests'

temp_dir="$(mktemp -d)"
readonly temp_dir
trap 'rm -rf "${temp_dir}"' EXIT

# --- every rendered Flux Kustomization prunes ---------------------------------

overlays=(
  k8s/clusters/prod
  k8s/clusters/local
)
for provider in hetzner docker; do
  for layer in bootstrap infrastructure/controllers infrastructure apps; do
    overlays+=("k8s/providers/${provider}/${layer}")
  done
done

: >"${temp_dir}/kustomizations.tsv"
for overlay in "${overlays[@]}"; do
  [[ -d "${root_dir}/${overlay}" ]] || fail "overlay ${overlay} does not exist; update this test's overlay list"
  rendered="${temp_dir}/${overlay//\//-}.yaml"
  kubectl kustomize "${root_dir}/${overlay}" >"${rendered}" ||
    fail "kubectl kustomize ${overlay} failed"
  yq eval -r '
    select(
      .kind == "Kustomization" and
      (.apiVersion // "" | test("^kustomize\.toolkit\.fluxcd\.io/"))
    ) |
    [
      (.metadata.namespace // "-"),
      .metadata.name,
      ((.spec.prune // "unset") | tostring)
    ] |
    @tsv
  ' "${rendered}" | sed "/^$/d; s#^#${overlay}	#" >>"${temp_dir}/kustomizations.tsv"
done

# The tenant ResourceGraphDefinition emits one Flux Kustomization per tenant;
# its template is nested, so the rendered-document select above cannot see it.
yq eval -r '
  .. |
  select(
    type == "!!map" and
    .kind == "Kustomization" and
    (.apiVersion // "" | test("^kustomize\.toolkit\.fluxcd\.io/"))
  ) |
  ["tenant-rgd", "-", (.metadata.name // "-"), ((.spec.prune // "unset") | tostring)] |
  @tsv
' "${tenant_rgd}" | sed '/^$/d' >>"${temp_dir}/kustomizations.tsv"

kustomization_count="$(wc -l <"${temp_dir}/kustomizations.tsv" | tr -d ' ')"
# Finding none means the selection broke, not that every tree prunes.
[[ "${kustomization_count}" -gt 0 ]] ||
  fail 'no Flux Kustomization was found in any rendered overlay; cannot check prune'
grep -q '	unifi	unifi	' "${temp_dir}/kustomizations.tsv" ||
  fail 'the unifi Flux Kustomization was not rendered; cannot check its protection'

not_pruning="$(awk -F '\t' '$4 != "true"' "${temp_dir}/kustomizations.tsv")"
[[ -z "${not_pruning}" ]] ||
  fail "every Flux Kustomization must set spec.prune: true (overlay, namespace, name, prune):
${not_pruning}"

# --- unifi retains live objects at the resource layer -------------------------

yq eval -e '.spec.patches | length > 0' "${unifi_kustomization}" >/dev/null 2>&1 ||
  fail 'the unifi Flux Kustomization declares no patches, so pruning would delete live UniFi objects'

# Flux appends spec.patches to the SOURCE's own root kustomization.yaml and sets
# its namespace from targetNamespace, so render exactly that. The source's own
# commonAnnotations and transformers run after the patches there, which is why
# the restrict-unifi-management-policies admission policy, not this render, is
# what stops a source from putting Delete back (see its kyverno fixtures).
render_unifi() {
  local source_dir="$1"
  local dir="${temp_dir}/unifi-$2"

  cp -R "${source_dir}" "${dir}"
  [[ -f "${dir}/kustomization.yaml" ]] ||
    fail "the $2 source has no root kustomization.yaml for Flux to patch"
  UNIFI_FLUX_KUSTOMIZATION="${unifi_kustomization}" yq eval -i '
    .namespace = "unifi" |
    .patches = ((.patches // []) + load(strenv(UNIFI_FLUX_KUSTOMIZATION)).spec.patches)
  ' "${dir}/kustomization.yaml"
  kubectl kustomize "${dir}" >"${dir}/rendered.yaml" ||
    fail "the unifi patches do not build over the $2 source"
  printf '%s\n' "${dir}/rendered.yaml"
}

# A UniFi managed resource must come out with exactly the retained policies, or
# only Observe when annotated observe-only; every other resource must come out
# without managementPolicies.
check_unifi_render() {
  local rendered="$1"
  local label="$2"
  local rows managed_count=0 kind name api managed mode policies want

  rows="$(UNIFI_OBSERVE_ANNOTATION="${observe_annotation}" yq eval -r '
    [
      .kind,
      .metadata.name,
      .apiVersion,
      ((.apiVersion | test("^[^/]+\.unifi\.m\.crossplane\.io/")) | tostring),
      (.metadata.annotations[strenv(UNIFI_OBSERVE_ANNOTATION)] // "-"),
      ((.spec.managementPolicies // []) | join(","))
    ] |
    @tsv
  ' "${rendered}" | sed '/^$/d')"
  [[ -n "${rows}" ]] || fail "the ${label} render produced no resources"

  # Tab is IFS whitespace, so consecutive tabs collapse; that is safe only
  # because policies, the one field that can be empty, is last.
  while IFS=$'\t' read -r kind name api managed mode policies; do
    if [[ "${managed}" == 'true' ]]; then
      managed_count=$((managed_count + 1))
      want="${retained_policies}"
      [[ "${mode}" != 'observe-only' ]] || want='Observe'
      [[ "${policies}" == "${want}" ]] ||
        fail "${label}: ${api} ${kind}/${name} has managementPolicies [${policies}], want [${want}]"
    else
      [[ -z "${policies}" ]] ||
        fail "${label}: ${api} ${kind}/${name} is not a UniFi managed resource but was patched to [${policies}]"
    fi
  done <<<"${rows}"

  [[ "${managed_count}" -gt 0 ]] ||
    fail "${label}: no UniFi managed resource was rendered; cannot check the protection"
  printf '%s\n' "${managed_count}"
}

mkdir -p "${temp_dir}/fixture"
printf 'resources:\n  - resources.yaml\n' >"${temp_dir}/fixture/kustomization.yaml"
cat >"${temp_dir}/fixture/resources.yaml" <<'YAML'
# No metadata.annotations and no managementPolicies: the patch must create both
# paths rather than fail on a missing map.
apiVersion: dns.unifi.m.crossplane.io/v1alpha1
kind: Record
metadata:
  name: bare-record
spec:
  forProvider:
    name: bare.example
    recordType: A
    value: 192.0.2.1
---
# A source that asks for Delete back must not keep it.
apiVersion: vpn.unifi.m.crossplane.io/v1alpha1
kind: Client
metadata:
  name: source-wants-delete
  annotations:
    crossplane.io/paused: "true"
spec:
  managementPolicies:
    - "*"
    - Delete
  forProvider: {}
---
apiVersion: route.unifi.m.crossplane.io/v1alpha1
kind: TrafficRoute
metadata:
  name: route
spec:
  forProvider: {}
---
# A group the provider serves but nothing activates yet: a later activation
# must be protected without editing the selector.
apiVersion: wlan.unifi.m.crossplane.io/v1alpha1
kind: APGroup
metadata:
  name: future-group
spec:
  forProvider: {}
---
# Adoption: whatever the source declares, an observe-only resource gets Observe.
apiVersion: dns.unifi.m.crossplane.io/v1alpha1
kind: Record
metadata:
  name: adopting
  annotations:
    crossplane.io/external-name: 0123456789abcdef01234567
    platform.devantler.tech/unifi-management: observe-only
spec:
  managementPolicies:
    - "*"
  forProvider: {}
---
# Only the exact value narrows; anything else keeps the retained policies.
apiVersion: dns.unifi.m.crossplane.io/v1alpha1
kind: Record
metadata:
  name: other-mode
  annotations:
    platform.devantler.tech/unifi-management: observe
spec:
  forProvider: {}
---
# The provider's own configuration group is not a managed resource.
apiVersion: unifi.m.crossplane.io/v1alpha1
kind: ProviderConfig
metadata:
  name: not-a-managed-resource
spec: {}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: not-a-managed-resource
data: {}
YAML

fixture_render="$(render_unifi "${temp_dir}/fixture" fixture)" || exit 1
fixture_managed="$(check_unifi_render "${fixture_render}" fixture)" || exit 1
[[ "${fixture_managed}" -eq 6 ]] ||
  fail "fixture: ${fixture_managed} managed resources were checked, want 6"

if [[ -n "${UNIFI_SOURCE_DIR:-}" ]]; then
  real_render="$(render_unifi "${UNIFI_SOURCE_DIR}" real)" || exit 1
  real_managed="$(check_unifi_render "${real_render}" "real source")" || exit 1
  printf 'unifi real source: %s managed resources retained\n' "${real_managed}"
fi

# Pruning runs as the namespaced unifi ServiceAccount; without delete on every
# managed group it grants, prune fails and the Kustomization never goes Ready.
missing_delete="$(yq eval -r '
  select(.kind == "Role") |
  .rules[] |
  select(.apiGroups[] | test("\.unifi\.m\.crossplane\.io$")) |
  select((.verbs | contains(["delete"])) | not) |
  .apiGroups |
  join(",")
' "${unifi_role}" | sed '/^$/d')"
[[ -z "${missing_delete}" ]] ||
  fail "the unifi Role must grant delete so Flux can prune: ${missing_delete}"

# An empty answer above must mean "every rule grants delete", not "no rule was
# read".
role_groups="$(yq eval -r '
  select(.kind == "Role") | .rules[].apiGroups[] | select(test("\.unifi\.m\.crossplane\.io$"))
' "${unifi_role}" | sed '/^$/d' | sort -u)"
[[ -n "${role_groups}" ]] ||
  fail 'the unifi Role grants no UniFi managed-resource group; cannot check prune'
role_resource_count="$(yq eval -r '
  select(.kind == "Role") | .rules[] |
  select(.apiGroups[] | test("\.unifi\.m\.crossplane\.io$")) | .resources[]
' "${unifi_role}" | sed '/^$/d' | sort -u | wc -l | tr -d ' ')"

# Every kind the unifi ServiceAccount may write must be one the admission policy
# guards, in every rule; otherwise a newly granted kind could be applied with
# Delete, or pruned while it still has it.
for rule in apply-without-delete observe-only-is-observe prune-only-without-delete; do
  policy_kinds="$(UNIFI_RULE="${rule}" yq eval -r '
    .spec.rules[] | select(.name == strenv(UNIFI_RULE)) | .match.any[].resources.kinds[]
  ' "${unifi_policy}" | sed '/^$/d' | sort -u)"
  [[ -n "${policy_kinds}" ]] ||
    fail "restrict-unifi-management-policies has no rule ${rule} with kinds"
  policy_groups="$(printf '%s\n' "${policy_kinds}" | cut -d/ -f1 | sort -u)"
  [[ "${policy_groups}" == "${role_groups}" ]] ||
    fail "rule ${rule} guards groups [${policy_groups//$'\n'/ }] but the unifi Role grants [${role_groups//$'\n'/ }]"
  policy_kind_count="$(printf '%s\n' "${policy_kinds}" | wc -l | tr -d ' ')"
  [[ "${policy_kind_count}" -eq "${role_resource_count}" ]] ||
    fail "rule ${rule} guards ${policy_kind_count} kinds but the unifi Role grants ${role_resource_count} resources"
done

printf 'flux kustomization prune: %s pruning, unifi retention holds on %s fixture resources\n' \
  "${kustomization_count}" "${fixture_managed}"
