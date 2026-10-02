#!/usr/bin/env bash
#
# Reports objects that Flux applied to the cluster but that no Flux Kustomization
# tracks any more, and fails on those a failed deploy left behind (#3502).
#
# Flux records every object a Kustomization applies in that Kustomization's
# inventory, and prunes an object when a later revision stops declaring it. An
# object carrying `kustomize.toolkit.fluxcd.io/prune: disabled` is the exception:
# Flux drops it from the inventory and leaves it running. Nothing updates or
# removes it after that.
#
# A merge-group deploy that fails after Flux has applied part of its revision
# leaves exactly that behind. On 2026-08-30 an evicted revision created a
# namespace and a HelmRelease that main did not declare. The heal job restored
# main and reported success, and the HelmRelease sat in prod at Ready=False for
# 38 hours until someone found it by hand. The heal runs this check after it has
# re-deployed main, so a green heal means nothing the failed revision applied is
# still running outside Flux.
#
# FLUX_ORPHANS_SINCE (a UTC timestamp, such as the merge group's creation time)
# separates that residue from older orphans. An orphan created at or after it
# can only have come from a revision applied since, and fails the check. An
# older one is reported but does not fail it: it is usually a prune-protected
# object whose manifest a retirement removed on purpose and which is waiting to
# be deleted by hand, and the prune-protected-orphan-alert CronJob escalates
# those once that window has passed (#3503). Without FLUX_ORPHANS_SINCE every
# orphan fails the check.
#
# What counts as an orphan: an object that kustomize-controller wrote (it holds a
# managedFields entry from that field manager), that carries the
# `kustomize.toolkit.fluxcd.io/name` label Flux puts on everything it applies, and
# whose inventory ID is in no Kustomization's inventory. Both markers are needed:
# controllers copy the label onto objects they derive (Endpoints and
# EndpointSlices from a Service, cert-manager Orders, Terraform state locks), and
# none of those has a kustomize-controller field manager. The inventory ID follows
# Flux's own encoding, `<namespace>_<name>_<group>_<kind>`, with `:` in the name
# of an RBAC Role, ClusterRole, RoleBinding or ClusterRoleBinding written as `__`.
#
# What is deliberately not reported, as in the CronJob: an object handed to
# another controller on purpose (annotated
# `platform.devantler.tech/prune-orphan: adopted`, or carrying ownerReferences),
# and an object already being deleted. An object whose Kustomization exists but
# is not Ready, or has recorded no inventory, cannot be judged, because Flux
# records the inventory only after a whole apply succeeds; that is UNKNOWN.
#
# The inventories must reflect the revision being checked: run this after a
# reconcile that waited for every Kustomization to apply it. The heal's deploy
# composite does, through `ksail workload reconcile`.
#
# Every listable resource type is read except the aggregated APIs, which an
# extension server serves rather than the API server. They hold no objects Flux
# applies here, and the largest of them ignores label selectors and returns every
# object it stores (about 17,000 scan results). The groups skipped are printed, so
# the scope of a read is never implicit.
#
# Flux applies an object before it records the new inventory, so an object
# created during a reconcile can be read before its Kustomization lists it.
# Objects are therefore read before the inventories, and a failing finding is
# reported only when a second read after FLUX_ORPHANS_SETTLE_SECONDS still finds
# the same object (by UID) outside every inventory.
#
#   exit 0  no orphan to fail on (older orphans, if any, are listed as warnings)
#   exit 1  an orphan to fail on: it is named, and nothing will update or prune it
#   exit 2  the cluster could not be read completely, nothing was examined, or an
#           object outside every inventory could not be judged
#
# Read-only. Every API request is bounded by a request timeout.

set -euo pipefail

kubectl_bin="${FLUX_ORPHANS_KUBECTL_BIN:-kubectl}"
context="${FLUX_ORPHANS_CONTEXT:-admin@prod}"
since="${FLUX_ORPHANS_SINCE:-}"
settle_seconds="${FLUX_ORPHANS_SETTLE_SECONDS:-60}"
retry_seconds="${FLUX_ORPHANS_RETRY_SECONDS:-20}"
readonly read_attempts=3
readonly request_timeout='60s'
readonly owner_label='kustomize.toolkit.fluxcd.io/name'
readonly owner_namespace_label='kustomize.toolkit.fluxcd.io/namespace'
readonly prune_annotation='kustomize.toolkit.fluxcd.io/prune'
readonly adopted_annotation='platform.devantler.tech/prune-orphan'
readonly flux_manager='kustomize-controller'

summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$@" >>"${GITHUB_STEP_SUMMARY}"
  fi
}

if ! [[ "${settle_seconds}" =~ ^[0-9]+$ && "${retry_seconds}" =~ ^[0-9]+$ ]]; then
  echo "::error::FLUX_ORPHANS_SETTLE_SECONDS and FLUX_ORPHANS_RETRY_SECONDS must be whole numbers of seconds."
  exit 2
fi
# Kubernetes writes creationTimestamp in UTC with a Z, so a UTC timestamp in the
# same form compares correctly as text. A merge-group commit timestamp may carry
# +00:00 instead; any other offset is refused rather than compared wrongly.
case "${since}" in
  *+00:00) since="${since%+00:00}Z" ;;
esac
if [[ -n "${since}" && ! "${since}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  echo "::error::FLUX_ORPHANS_SINCE '${FLUX_ORPHANS_SINCE}' is not a UTC timestamp."
  exit 2
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

kubectl_cluster() {
  "${kubectl_bin}" --context "${context}" --request-timeout="${request_timeout}" "$@"
}

unknown() {
  echo "::error::Could not tell whether Flux left objects outside every inventory: $1"
  if [[ -s "${tmp_dir}/error.log" ]]; then
    head -c 2000 "${tmp_dir}/error.log"
    echo
  fi
  summary "- Flux orphaned objects: UNKNOWN — $1"
  exit 2
}

# read_state <dir>: the objects first, then the inventories (see the header).
read_state() {
  local dir="$1" resource group listed=''
  mkdir -p "${dir}"
  kubectl_cluster get apiservices.apiregistration.k8s.io -o json \
    >"${dir}/apiservices.json" 2>"${tmp_dir}/error.log" || return 1
  jq -r '.items[] | select(.spec.service != null) | .spec.group' "${dir}/apiservices.json" \
    >"${dir}/aggregated.unsorted" 2>"${tmp_dir}/error.log" || return 1
  sort -u "${dir}/aggregated.unsorted" >"${dir}/aggregated.txt"
  kubectl_cluster api-resources --verbs=list -o name \
    >"${dir}/resources.txt" 2>"${tmp_dir}/error.log" || return 1
  # `kubectl api-resources -o name` prints `<plural>.<group>`, or `<plural>` for
  # the core group. A plural never contains a dot.
  while IFS= read -r resource; do
    [[ -n "${resource}" ]] || continue
    group=''
    if [[ "${resource}" == *.* ]]; then
      group="${resource#*.}"
    fi
    if [[ -n "${group}" ]] && grep -Fxq -- "${group}" "${dir}/aggregated.txt"; then
      continue
    fi
    listed+="${listed:+,}${resource}"
  done <"${dir}/resources.txt"
  if [[ -z "${listed}" ]]; then
    echo "the API server listed no resource types" >"${tmp_dir}/error.log"
    return 1
  fi
  kubectl_cluster get "${listed}" --all-namespaces -l "${owner_label}" \
    --show-managed-fields -o json >"${dir}/objects.json" 2>"${tmp_dir}/error.log" || return 1
  kubectl_cluster get kustomizations.kustomize.toolkit.fluxcd.io --all-namespaces -o json \
    >"${dir}/kustomizations.json" 2>"${tmp_dir}/error.log" || return 1
}

# read_with_retry <dir>: a deploy that just finished can still be restarting an
# API server or an aggregated API, which fails discovery for a moment.
read_with_retry() {
  local attempt
  for ((attempt = 1; attempt <= read_attempts; attempt++)); do
    if read_state "$1"; then
      return 0
    fi
    if ((attempt < read_attempts)); then
      echo "Read ${attempt}/${read_attempts} failed; retrying in ${retry_seconds}s."
      sleep "${retry_seconds}"
    fi
  done
  return 1
}

# Writes one line per object outside every inventory, then the counts the read
# examined:
#   <category> <uid> <inventory id> <object> claims=<ns/name> created=<time> prune=<annotation|-> [reason=<why>]
#   checked=<Flux-applied objects evaluated>
#   kustomizations=<Kustomizations read>
# where <category> is `orphan` (fails the check), `pre-existing` (an orphan
# created before $since) or `unjudged` (its Kustomization is not Ready or has no
# inventory).
# shellcheck disable=SC2016 # jq program, not shell expansion
readonly evaluate_program='
  def rbac_kind: . as $k | ["Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding"] | any(. == $k);
  def group_of: .apiVersion | if test("/") then split("/")[0] else "" end;
  def inventory_id:
    group_of as $g
    | (if $g == "rbac.authorization.k8s.io" and (.kind | rbac_kind)
       then .metadata.name | gsub(":"; "__")
       else .metadata.name end) as $name
    | "\(.metadata.namespace // "")_\($name)_\($g)_\(.kind)";
  def object_label:
    "\(.kind)\(group_of | if . == "" then "" else ".\(.)" end) \(.metadata.namespace // "" | if . == "" then "" else "\(.)/" end)\(.metadata.name)";
  ($kustomizations[0].items // []) as $ks
  | (reduce ($ks[] | .status.inventory.entries[]?.id) as $id ({}; .[$id] = true)) as $inventory
  | (reduce $ks[] as $k ({};
      .["\($k.metadata.namespace)/\($k.metadata.name)"] = {
        ready: any($k.status.conditions[]?; .type == "Ready" and .status == "True"),
        inventory: ($k.status.inventory != null)
      })) as $owners
  | [($objects[0].items // [])[]
     | select((.metadata.labels // {})[$owner_label] != null)
     | select(any(.metadata.managedFields[]?; .manager == $manager))] as $applied
  | ($applied[]
     | select(.metadata.deletionTimestamp == null)
     | select((.metadata.annotations // {})[$adopted_annotation] != "adopted")
     | select((.metadata.ownerReferences // []) | length == 0)
     | inventory_id as $id
     | select($inventory[$id] | not)
     | "\(.metadata.labels[$owner_namespace_label] // "")/\(.metadata.labels[$owner_label])" as $claims
     | $owners[$claims] as $owner
     | (if $owner == null then ""
        elif ($owner.inventory | not) then "reason=no-inventory"
        elif ($owner.ready | not) then "reason=not-ready"
        else "" end) as $unjudged
     | (if $unjudged != "" then "unjudged"
        elif $since != "" and .metadata.creationTimestamp < $since then "pre-existing"
        else "orphan" end) as $category
     | "\($category) \(.metadata.uid) \($id) \(object_label) claims=\($claims) created=\(.metadata.creationTimestamp) prune=\((.metadata.annotations // {})[$prune_annotation] // "-")\(if $unjudged == "" then "" else " \($unjudged)" end)"),
    "checked=\($applied | length)",
    "kustomizations=\($ks | length)"
'

# evaluate <dir>: writes <dir>/findings.txt and fails when the read examined nothing.
evaluate() {
  local dir="$1" checked kustomizations
  jq -r -n \
    --arg owner_label "${owner_label}" \
    --arg owner_namespace_label "${owner_namespace_label}" \
    --arg prune_annotation "${prune_annotation}" \
    --arg adopted_annotation "${adopted_annotation}" \
    --arg manager "${flux_manager}" \
    --arg since "${since}" \
    --slurpfile objects "${dir}/objects.json" \
    --slurpfile kustomizations "${dir}/kustomizations.json" \
    "${evaluate_program}" >"${dir}/result.txt" 2>"${tmp_dir}/error.log" || return 1
  checked="$(sed -n 's/^checked=//p' "${dir}/result.txt")"
  kustomizations="$(sed -n 's/^kustomizations=//p' "${dir}/result.txt")"
  # A read that saw no Kustomization or no Flux-applied object examined nothing,
  # so it can never report a clean cluster.
  if ! [[ "${checked}" =~ ^[0-9]+$ && "${kustomizations}" =~ ^[0-9]+$ ]] ||
    ((checked == 0 || kustomizations == 0)); then
    printf 'the read found %s Kustomization(s) and %s Flux-applied object(s)\n' \
      "${kustomizations:-no}" "${checked:-no}" >"${tmp_dir}/error.log"
    return 1
  fi
  grep -E '^(orphan|pre-existing|unjudged) ' "${dir}/result.txt" >"${dir}/findings.txt" || true
}

# print_findings <file>: one indented line per finding, without the UID and ID.
print_findings() {
  local line object_kind object_name rest
  while IFS= read -r line; do
    read -r _ _ _ object_kind object_name rest <<<"${line}"
    printf '  %s %s (%s)\n' "${object_kind}" "${object_name}" "${rest}"
  done <"$1"
}

skipped_groups() {
  if [[ -s "$1/aggregated.txt" ]]; then
    paste -s -d, "$1/aggregated.txt" | sed 's/,/, /g'
  else
    echo none
  fi
}

# report_pre_existing <file>: older orphans, listed but not failed on.
report_pre_existing() {
  local line
  [[ -s "$1" ]] || return 0
  echo "::warning::$(wc -l <"$1" | tr -d ' ') object(s) outside every inventory predate ${since}, so no deploy since then left them; they are not failed on here:"
  print_findings "$1"
  echo "If a retirement removed their manifests on purpose, delete them by hand once nothing depends on them (AGENTS.md, \"Persistence retirement is always two-stage\"); the prune-protected-orphan-alert CronJob escalates them after its grace period (#3503)."
  summary "- Flux orphaned objects older than ${since}, not failed on:"
  while IFS= read -r line; do
    summary "  - \`$(cut -d' ' -f4-5 <<<"${line}")\` ($(cut -d' ' -f6- <<<"${line}"))"
  done <"$1"
}

scope() {
  local subject="all Flux-applied objects"
  if [[ -n "${since}" ]]; then
    subject="Flux-applied objects created since ${since}"
  fi
  printf '%s' "${subject}"
}

first="${tmp_dir}/first"
read_with_retry "${first}" || unknown "the cluster could not be read."
evaluate "${first}" || unknown "the first read examined nothing."
checked="$(sed -n 's/^checked=//p' "${first}/result.txt")"
kustomizations="$(sed -n 's/^kustomizations=//p' "${first}/result.txt")"
grep '^pre-existing ' "${first}/findings.txt" >"${tmp_dir}/pre-existing-first.txt" || true

if ! grep -Eq '^(orphan|unjudged) ' "${first}/findings.txt"; then
  report_pre_existing "${tmp_dir}/pre-existing-first.txt"
  echo "✅ No object is outside every inventory among $(scope) (${checked} Flux-applied objects, ${kustomizations} Kustomizations; aggregated API groups not read: $(skipped_groups "${first}"))."
  summary "- Flux orphaned objects: none among $(scope) — ${checked} Flux-applied objects, ${kustomizations} Kustomizations."
  exit 0
fi

echo "$(grep -Ec '^(orphan|unjudged) ' "${first}/findings.txt") object(s) are outside every inventory; reading again in ${settle_seconds}s to rule out a reconcile in progress."
sleep "${settle_seconds}"

second="${tmp_dir}/second"
read_with_retry "${second}" || unknown "the cluster could not be read a second time."
evaluate "${second}" || unknown "the second read examined nothing."

# A finding is confirmed when the same object (UID and inventory ID) is outside
# every inventory in both reads; its category is the one the second read gives.
# A finding only the second read has appeared in between, so it is not confirmed.
cut -d' ' -f2-3 "${first}/findings.txt" | sort -u >"${tmp_dir}/first-keys.txt"
: >"${tmp_dir}/confirmed.txt"
: >"${tmp_dir}/unconfirmed.txt"
while IFS= read -r line; do
  if grep -Fxq -- "$(cut -d' ' -f2-3 <<<"${line}")" "${tmp_dir}/first-keys.txt"; then
    printf '%s\n' "${line}" >>"${tmp_dir}/confirmed.txt"
  else
    printf '%s\n' "${line}" >>"${tmp_dir}/unconfirmed.txt"
  fi
done <"${second}/findings.txt"
grep '^orphan ' "${tmp_dir}/confirmed.txt" >"${tmp_dir}/orphans.txt" || true
grep '^unjudged ' "${tmp_dir}/confirmed.txt" >"${tmp_dir}/unjudged.txt" || true
grep '^pre-existing ' "${second}/findings.txt" >"${tmp_dir}/pre-existing.txt" || true
grep -Ev '^pre-existing ' "${tmp_dir}/unconfirmed.txt" >"${tmp_dir}/unconfirmed-new.txt" || true

report_pre_existing "${tmp_dir}/pre-existing.txt"

if [[ -s "${tmp_dir}/orphans.txt" ]]; then
  count="$(wc -l <"${tmp_dir}/orphans.txt" | tr -d ' ')"
  echo "::error::${count} object(s) among $(scope) are in no Kustomization's inventory, so nothing will update or prune them:"
  print_findings "${tmp_dir}/orphans.txt"
  cat <<'EOF'
Flux drops an object from its inventory when a revision stops declaring it, and
deletes it unless it carries kustomize.toolkit.fluxcd.io/prune: disabled. These
were left by a revision that applied them and was then replaced, such as a
merge-group deploy that failed or was evicted (#3502). Delete each one, or
declare it in Git again so its Kustomization adopts it.
EOF
  summary "- Flux orphaned objects: **${count} found** among $(scope) — in no Kustomization inventory:"
  while IFS= read -r line; do
    summary "  - \`$(cut -d' ' -f4-5 <<<"${line}")\` ($(cut -d' ' -f6- <<<"${line}"))"
  done <"${tmp_dir}/orphans.txt"
  if [[ -s "${tmp_dir}/unjudged.txt" ]]; then
    echo "These objects could not be judged, because their Kustomization is not Ready or has recorded no inventory:"
    print_findings "${tmp_dir}/unjudged.txt"
  fi
  exit 1
fi

if [[ -s "${tmp_dir}/unjudged.txt" ]]; then
  echo "These objects are outside every inventory, but their Kustomization is not Ready or has recorded no inventory, so they cannot be judged:"
  print_findings "${tmp_dir}/unjudged.txt"
  unknown "a Kustomization that claims objects outside every inventory is not Ready or has no inventory."
fi

cleared="$(grep -Ec '^(orphan|unjudged) ' "${first}/findings.txt")"
checked="$(sed -n 's/^checked=//p' "${second}/result.txt")"
kustomizations="$(sed -n 's/^kustomizations=//p' "${second}/result.txt")"
echo "✅ No object stayed outside every inventory across both reads among $(scope): the ${cleared} found on the first read were in an inventory by the second (${checked} Flux-applied objects, ${kustomizations} Kustomizations; aggregated API groups not read: $(skipped_groups "${second}"))."
if [[ -s "${tmp_dir}/unconfirmed-new.txt" ]]; then
  echo "The second read found these outside every inventory for the first time, so they are not confirmed and are expected to be a reconcile in progress:"
  print_findings "${tmp_dir}/unconfirmed-new.txt"
fi
summary "- Flux orphaned objects: none confirmed among $(scope) — ${checked} Flux-applied objects, ${kustomizations} Kustomizations (${cleared} cleared on re-read)."
exit 0
