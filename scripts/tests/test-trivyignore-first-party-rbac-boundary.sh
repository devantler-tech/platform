#!/usr/bin/env bash
# The first-party RBAC dispositions in .trivyignore.yaml rest on facts about how each role is
# BOUND and what it can do — not on the rules trivy reads. This test is what keeps those facts true.
#
# WHY THIS EXISTS
# trivy evaluates a ClusterRole's rules in isolation, so it reports every grant as cluster-wide.
# Four first-party roles are dispositioned in .trivyignore.yaml (#2990) because a fact outside the
# rules makes the grant safe:
#
#   tenant-base-edit  — aggregated into tenant-edit and bound ONLY by a namespace-scoped
#                       RoleBinding, so its secrets and networking grants cannot leave the tenant's
#                       own namespace.
#   cluster-reader    — its non-core rule uses resources ["*"], but every rule grants
#                       get/list/watch only, and the core ("") group enumerates its
#                       resources so Secrets are out of reach.
#   kro-*-rgd         — aggregated into a KRO controller that runs with rbac.mode `aggregation` and
#                       therefore holds no standing access of its own.
#
# Each of those is a PREMISE, and a disposition that outlives its premise is a silent hole: the
# finding stays suppressed while the reason for suppressing it has gone. Nothing else would fail —
# the scan still runs and the count does not move, which reads exactly like nothing happened.
#
# So this test asserts the premises, not the prose:
#   * no ClusterRoleBinding anywhere under k8s/ binds the tenant role (checked recursively, so a
#     binding nested inside a ResourceGraphDefinition template counts too);
#   * cluster-reader grants no verb outside get/list/watch, and its core-group rules never
#     wildcard resources or name secrets;
#   * KRO keeps rbac.mode `aggregation` and both RGD roles keep the aggregation label;
#   * each disposition stays scoped to its own file, never loses its paths key, and no pair beyond
#     the reviewed matrix appears.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly ignorefile="$repo_root/.trivyignore.yaml"

command -v yq >/dev/null 2>&1 || {
  printf 'yq is required to check the first-party RBAC disposition premises\n' >&2
  exit 1
}

readonly tenant_role='k8s/bases/infrastructure/cluster-roles/tenant-base-edit.yaml'
readonly reader_role='k8s/bases/infrastructure/cluster-roles/cluster-reader.yaml'
readonly kro_tenant='k8s/bases/infrastructure/resource-graph-definitions/tenant/cluster-role.yaml'
readonly kro_webapp='k8s/bases/infrastructure/resource-graph-definitions/webapp/cluster-role.yaml'
readonly kro_release='k8s/bases/infrastructure/controllers/kro/helm-release.yaml'

status=0

# ---------------------------------------------------------------- structure --
# id:path pairs that must exist, each scoped to exactly that one file.
readonly pairs=(
  "KSV-0041:$tenant_role"
  "KSV-0056:$tenant_role"
  "KSV-0046:$reader_role"
)
for pair in "${pairs[@]}"; do
  check="${pair%%:*}"
  path="${pair#*:}"
  n="$(yq "[.misconfigurations[] | select(.id == \"$check\") | select((.paths // []) | contains([\"$path\"]))] | length" "$ignorefile" 2>/dev/null || printf '0')"
  if [ "${n:-0}" -lt 1 ]; then
    printf 'MISSING DISPOSITION: %s is not dispositioned for %s\n' "$check" "$path" >&2
    status=1
  fi
done

# The KRO pair is one entry covering both RGD roles.
kro_n="$(yq "[.misconfigurations[] | select(.id == \"KSV-0056\") | select((.paths // []) | contains([\"$kro_tenant\", \"$kro_webapp\"]))] | length" "$ignorefile" 2>/dev/null || printf '0')"
if [ "${kro_n:-0}" -lt 1 ]; then
  printf 'MISSING DISPOSITION: KSV-0056 does not cover both KRO RGD cluster roles\n' >&2
  status=1
fi

# No entry naming a first-party role may be unscoped, which would suppress it everywhere.
while IFS= read -r n; do
  if [ "${n:-1}" -eq 0 ]; then
    printf 'UNSCOPED SKIP: a first-party RBAC entry has no paths key\n' >&2
    status=1
  fi
done < <(yq '.misconfigurations[] | select(.id == "KSV-0041" or .id == "KSV-0046" or .id == "KSV-0056") | (.paths // []) | length' "$ignorefile")

# Nothing beyond the reviewed matrix. The check ids here are also dispositioned for the two vendored
# operator bundles, so those two paths are expected; ANY other first-party pair is an unreviewed
# suppression, and it must fail here rather than being noticed only if someone re-reads the file.
readonly reviewed_pairs=(
  "KSV-0041:$tenant_role"
  "KSV-0056:$tenant_role"
  "KSV-0056:$kro_tenant"
  "KSV-0056:$kro_webapp"
  "KSV-0046:$reader_role"
)
readonly vendored_cdi='k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml'
readonly vendored_kubevirt='k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml'
while IFS= read -r pair; do
  [ -n "$pair" ] || continue
  pair_path="${pair#*:}"
  [ "$pair_path" = "$vendored_cdi" ] && continue
  [ "$pair_path" = "$vendored_kubevirt" ] && continue
  known=0
  for rp in "${reviewed_pairs[@]}"; do
    if [ "$pair" = "$rp" ]; then
      known=1
      break
    fi
  done
  if [ "$known" -eq 0 ]; then
    printf 'UNREVIEWED DISPOSITION: %s is not one of the reviewed first-party verdicts\n' "$pair" >&2
    status=1
  fi
done < <(yq -N '.misconfigurations[] | select(.id == "KSV-0041" or .id == "KSV-0046" or .id == "KSV-0053" or .id == "KSV-0056" or .id == "KSV-0114") | .id + ":" + (.paths // [])[]' "$ignorefile")

# ----------------------------------------------------------------- premises --
# 1. The tenant role is never bound cluster-wide. Recursive, so a ClusterRoleBinding nested in an
#    RGD resource template is caught as well as a top-level one.
bindings=0
while IFS= read -r -d '' file; do
  found="$(yq -N "[.. | select(tag == \"!!map\") | select(.kind == \"ClusterRoleBinding\") | select((.roleRef.name // \"\") == \"tenant-edit\" or (.roleRef.name // \"\") == \"tenant-base-edit\")] | length" "$file" 2>/dev/null || printf '0')"
  # A multi-document file yields one count per document.
  for c in $found; do
    [ "${c:-0}" -gt 0 ] && bindings=$((bindings + c))
  done
done < <(find "$repo_root/k8s" -type f \( -name '*.yaml' -o -name '*.yml' \) -print0)
if [ "$bindings" -ne 0 ]; then
  printf 'PREMISE BROKEN: %d ClusterRoleBinding(s) bind the tenant role; the KSV-0041/KSV-0056 dispositions on %s assume namespace-scoped RoleBindings only\n' "$bindings" "$tenant_role" >&2
  status=1
fi

# 2. cluster-reader stays read-only.
while IFS= read -r verb; do
  [ -n "$verb" ] || continue
  case "$verb" in
    get | list | watch) ;;
    *)
      printf 'PREMISE BROKEN: cluster-reader grants verb %s; the KSV-0046 disposition assumes get/list/watch only\n' "$verb" >&2
      status=1
      ;;
  esac
done < <(yq -N '.. | select(tag == "!!map") | select(.kind == "ClusterRole") | select(.metadata.name == "cluster-reader") | .rules[].verbs[]' "$repo_root/$reader_role")

# 2b. cluster-reader's CORE-group rules stay resource-enumerated.
#     Read-only is necessary but NOT sufficient: KSV-0046 is about breadth, and a get/list/watch
#     grant on core `secrets` would read every Secret in the cluster while passing check 2 above.
#     The protection is that the core ("") group enumerates its resources instead of wildcarding —
#     so assert exactly that, rather than trusting the prose. Non-core groups may wildcard
#     resources; the core group is where Secrets live.
while IFS= read -r rule; do
  [ -n "$rule" ] || continue
  groups="${rule%%|*}"
  resources="${rule#*|}"
  core=0
  old_ifs="$IFS"
  set -f # a resource may literally be "*"; without this, word-splitting globs it away
  IFS=','
  for g in $groups; do
    [ -z "$g" ] && core=1
  done
  # A rule whose apiGroups list is exactly [""] joins to the empty string, so the loop above sees
  # no fields at all; catch that case explicitly.
  [ -z "$groups" ] && core=1
  for r in $resources; do
    if [ "$core" -eq 1 ] && { [ "$r" = '*' ] || [ "$r" = 'secrets' ]; }; then
      IFS="$old_ifs"
      printf 'PREMISE BROKEN: cluster-reader grants core-group resource %s; the KSV-0046 disposition assumes the core group enumerates resources so Secrets stay out of reach\n' "$r" >&2
      status=1
      IFS=','
    fi
  done
  IFS="$old_ifs"
  set +f
done < <(yq -N '.. | select(tag == "!!map") | select(.kind == "ClusterRole") | select(.metadata.name == "cluster-reader") | .rules[] | ((.apiGroups // []) | join(",")) + "|" + ((.resources // []) | join(","))' "$repo_root/$reader_role")

# 3. KRO holds no standing access, and both RGD roles are aggregated into it.
mode="$(yq -N '.spec.values.rbac.mode // ""' "$repo_root/$kro_release" 2>/dev/null || printf '')"
if [ "$mode" != "aggregation" ]; then
  printf 'PREMISE BROKEN: KRO rbac.mode is %s, not aggregation; the KSV-0056 disposition on the RGD roles assumes the controller has no standing access\n' "${mode:-unset}" >&2
  status=1
fi
for role in "$kro_tenant" "$kro_webapp"; do
  label="$(yq -N '.metadata.labels["rbac.kro.run/aggregate-to-controller"] // ""' "$repo_root/$role" 2>/dev/null || printf '')"
  if [ "$label" != "true" ]; then
    printf 'PREMISE BROKEN: %s no longer carries the KRO aggregation label\n' "$role" >&2
    status=1
  fi
done

[ "$status" -eq 0 ] || exit 1
printf 'first-party RBAC dispositions hold: tenant role never cluster-bound, cluster-reader read-only, KRO aggregation-scoped\n'
