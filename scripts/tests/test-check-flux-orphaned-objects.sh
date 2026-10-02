#!/usr/bin/env bash
# Tests for scripts/check-flux-orphaned-objects.sh (#3502) against a fake kubectl
# that serves fixture files. No cluster and no credentials.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo_root}/scripts/check-flux-orphaned-objects.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

# The fake refuses any call that is not pinned to admin@prod with the bounded
# request timeout, and any call that is not one of the four reads the check
# makes, so an unbounded or mutating call fails the test rather than a deploy.
# Each read starts with the APIService list, which therefore counts the reads;
# `<name>.<read>.json` overrides `<name>.json` for that read. kubectl prints
# warnings on stderr even when a read succeeds, and so does the fake.
fake_kubectl="${tmp_dir}/kubectl"
cat >"${fake_kubectl}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

[[ "$1 $2 $3" == "--context admin@prod --request-timeout=60s" ]] || {
  printf 'unexpected or unbounded kubectl call: %s\n' "$*" >&2
  exit 91
}
shift 3

read_number=0
if [[ -f "${FAKE_DIR}/reads" ]]; then
  read_number="$(<"${FAKE_DIR}/reads")"
fi

serve() {
  if [[ -f "${FAKE_DIR}/$1.${read_number}.json" ]]; then
    cat "${FAKE_DIR}/$1.${read_number}.json"
  else
    cat "${FAKE_DIR}/$1.json"
  fi
}

if [[ "$*" == "get apiservices.apiregistration.k8s.io -o json" ]]; then
  read_number=$((read_number + 1))
  printf '%s\n' "${read_number}" >"${FAKE_DIR}/reads"
  serve apiservices
  exit 0
fi

if [[ "$*" == "api-resources --verbs=list -o name" ]]; then
  cat "${FAKE_DIR}/resources.txt"
  exit 0
fi

if [[ "$#" -eq 8 && "$1" == get && "$3 $4 $5 $6 $7 $8" == "--all-namespaces -l kustomize.toolkit.fluxcd.io/name --show-managed-fields -o json" ]]; then
  printf '%s\n' "$2" >"${FAKE_DIR}/listed.${read_number}"
  printf 'Warning: v1 Endpoints is deprecated in v1.33+\n' >&2
  if [[ ",${FAKE_FAIL_READS:-}," == *",${read_number},"* ]]; then
    printf 'The connection to the server was refused\n' >&2
    exit 1
  fi
  serve objects
  exit 0
fi

if [[ "$*" == "get kustomizations.kustomize.toolkit.fluxcd.io --all-namespaces -o json" ]]; then
  printf 'Warning: kustomize.toolkit.fluxcd.io/v1beta2 Kustomization is deprecated\n' >&2
  serve kustomizations
  exit 0
fi

printf 'unexpected kubectl arguments: %s\n' "$*" >&2
exit 92
EOF
chmod +x "${fake_kubectl}"

readonly prune_disabled='{"annotations":{"kustomize.toolkit.fluxcd.io/prune":"disabled"}}'
readonly applied_at='2026-08-30T14:17:34Z'

# obj <apiVersion> <kind> <namespace|-> <name> <uid> <claiming ks ns/name|-> <field manager> [metadata JSON]
# One object as `kubectl get --show-managed-fields -o json` returns it, created
# at $applied_at unless the extra metadata says otherwise.
obj() {
  local extra="${8:-}"
  [[ -n "${extra}" ]] || extra='{}'
  jq -cn --arg apiVersion "$1" --arg kind "$2" --arg ns "$3" --arg name "$4" --arg uid "$5" \
    --arg claims "$6" --arg manager "$7" --argjson extra "${extra}" --arg created "${applied_at}" '
    {apiVersion: $apiVersion, kind: $kind,
     metadata: ({name: $name, uid: $uid, creationTimestamp: $created,
                 managedFields: [{manager: $manager, operation: "Apply"}]}
                + (if $ns == "-" then {} else {namespace: $ns} end)
                + (if $claims == "-" then {} else {labels: {
                    "kustomize.toolkit.fluxcd.io/name": ($claims | split("/")[1]),
                    "kustomize.toolkit.fluxcd.io/namespace": ($claims | split("/")[0])}} end)
                + $extra)}'
}

# ks <namespace> <name> <Ready status> [inventory ids...] — "none" as the only id
# records no inventory.
ks() {
  local ns="$1" name="$2" ready="$3"
  shift 3
  jq -cn --arg ns "${ns}" --arg name "${name}" --arg ready "${ready}" '
    {metadata: {namespace: $ns, name: $name},
     status: ({conditions: [{type: "Ready", status: $ready}]}
              + (if $ARGS.positional == ["none"] then {}
                 else {inventory: {entries: [$ARGS.positional[] | {id: ., v: "v1"}]}} end))}' \
    --args "$@"
}

# items <file>: the JSON objects on stdin become that file's list.
items() {
  jq -s '{apiVersion: "v1", kind: "List", items: .}' >"$1"
}

# scenario <name>: a fixture directory with the resource types and APIServices of
# a cluster that serves one aggregated API, and four Flux-applied objects, each
# recorded in an inventory.
scenario() {
  local dir="${tmp_dir}/$1"
  mkdir -p "${dir}"
  cat >"${dir}/resources.txt" <<'RESOURCES'
namespaces
configmaps
endpoints
clusterroles.rbac.authorization.k8s.io
endpointslices.discovery.k8s.io
helmreleases.helm.toolkit.fluxcd.io
ciliumnetworkpolicies.cilium.io
orders.acme.cert-manager.io
containerprofiles.spdx.softwarecomposition.kubescape.io
RESOURCES
  jq -n '{items: [
    {metadata: {name: "v1.apps"}, spec: {group: "apps", service: null}},
    {metadata: {name: "v1beta1.spdx.softwarecomposition.kubescape.io"},
     spec: {group: "spdx.softwarecomposition.kubescape.io", service: {namespace: "kubescape", name: "storage"}}}]}' \
    >"${dir}/apiservices.json"
  {
    obj v1 Namespace - web uid-ns-web flux-system/apps kustomize-controller "${prune_disabled}"
    obj helm.toolkit.fluxcd.io/v2 HelmRelease web web uid-hr-web flux-system/apps kustomize-controller "${prune_disabled}"
    obj v1 ConfigMap web settings uid-cm-web flux-system/apps kustomize-controller
    obj rbac.authorization.k8s.io/v1 ClusterRole - kyverno:reports-controller:read-nodes uid-cr \
      flux-system/infrastructure-controllers kustomize-controller
  } | items "${dir}/objects.json"
  {
    ks flux-system apps True _web__Namespace web_web_helm.toolkit.fluxcd.io_HelmRelease web_settings__ConfigMap
    ks flux-system infrastructure-controllers True \
      _kyverno__reports-controller__read-nodes_rbac.authorization.k8s.io_ClusterRole
  } | items "${dir}/kustomizations.json"
  printf '%s\n' "${dir}"
}

# add_objects <dir> [read]: append the objects on stdin to that read's object list.
add_objects() {
  local dir="$1" target="$1/objects.json"
  if [[ -n "${2:-}" ]]; then
    target="${dir}/objects.$2.json"
    [[ -f "${target}" ]] || cp "${dir}/objects.json" "${target}"
  fi
  jq -s '.[0].items += .[1:] | .[0]' "${target}" - >"${target}.new"
  mv "${target}.new" "${target}"
}

# add_kustomizations <file>: append the Kustomizations on stdin to that list.
add_kustomizations() {
  jq -s '.[0].items += .[1:] | .[0]' "$1" - >"$1.new"
  mv "$1.new" "$1"
}

# record <file> <id>: add an inventory entry to the apps Kustomization in that list.
record() {
  jq --arg id "$2" '(.items[] | select(.metadata.name == "apps") | .status.inventory.entries) += [{id: $id, v: "v1"}]' \
    "$1" >"$1.new"
  mv "$1.new" "$1"
}

run() {
  local dir="$1"
  shift
  : >"${dir}/summary.md"
  rm -f "${dir}/reads"
  set +e
  output="$(env \
    FLUX_ORPHANS_KUBECTL_BIN="${fake_kubectl}" \
    FLUX_ORPHANS_SETTLE_SECONDS=0 \
    FLUX_ORPHANS_RETRY_SECONDS=0 \
    GITHUB_STEP_SUMMARY="${dir}/summary.md" \
    FAKE_DIR="${dir}" \
    "$@" bash "${script}" 2>&1)"
  status=$?
  set -e
}

fail() {
  printf 'FAIL %s: %s\n--- output ---\n%s\n' "${case_name}" "$1" "${output}" >&2
  exit 1
}

expect_status() {
  [[ "${status}" -eq "$1" ]] || fail "expected exit $1, got ${status}"
}

expect_line() {
  grep -Fxq -- "$1" <<<"${output}" || fail "missing line: $1"
}

expect_text() {
  grep -Fq -- "$1" <<<"${output}" || fail "missing text: $1"
}

expect_no_text() {
  if grep -Fq -- "$1" <<<"${output}"; then
    fail "unexpected text: $1"
  fi
}

expect_summary() {
  grep -Fq -- "$1" "${dir}/summary.md" || fail "the step summary is missing: $1"
}

expect_reads() {
  local reads
  reads="$(cat "${dir}/reads")"
  [[ "${reads}" == "$1" ]] || fail "expected $1 read(s), got ${reads}"
}

# Every Flux-applied object is in an inventory, including an RBAC name whose
# colons Flux writes as double underscores. One read is enough.
case_name='clean cluster'
dir="$(scenario clean)"
run "${dir}"
expect_status 0
expect_line '✅ No object is outside every inventory among all Flux-applied objects (4 Flux-applied objects, 2 Kustomizations; aggregated API groups not read: spdx.softwarecomposition.kubescape.io).'
expect_reads 1
expect_summary '- Flux orphaned objects: none among all Flux-applied objects — 4 Flux-applied objects, 2 Kustomizations.'

# The aggregated API is never listed; every other listable type is, in one call.
case_name='aggregated APIs are not read'
listed="$(cat "${dir}/listed.1")"
[[ "${listed}" == 'namespaces,configmaps,endpoints,clusterroles.rbac.authorization.k8s.io,endpointslices.discovery.k8s.io,helmreleases.helm.toolkit.fluxcd.io,ciliumnetworkpolicies.cilium.io,orders.acme.cert-manager.io' ]] ||
  fail "unexpected resource list: ${listed}"

# The #3502 incident: an evicted revision's namespace and HelmRelease, and a
# network policy, stay in prod after main is restored. The HelmRelease and the
# namespace carry prune: disabled, so Flux dropped them from its inventory.
case_name='residue of a failed deploy'
dir="$(scenario residue)"
{
  obj v1 Namespace - data-product-controller uid-ns-dpc flux-system/apps kustomize-controller "${prune_disabled}"
  obj helm.toolkit.fluxcd.io/v2 HelmRelease data-product-controller data-product-controller uid-hr-dpc \
    flux-system/apps kustomize-controller "${prune_disabled}"
  obj cilium.io/v2 CiliumNetworkPolicy data-product-controller allow-data-product-controller uid-cnp-dpc \
    flux-system/apps kustomize-controller
} | add_objects "${dir}"
run "${dir}" FLUX_ORPHANS_SINCE=2026-08-30T14:05:12Z
expect_status 1
expect_reads 2
expect_text "::error::3 object(s) among Flux-applied objects created since 2026-08-30T14:05:12Z are in no Kustomization's inventory, so nothing will update or prune them:"
expect_line '  Namespace data-product-controller (claims=flux-system/apps created=2026-08-30T14:17:34Z prune=disabled)'
expect_line '  HelmRelease.helm.toolkit.fluxcd.io data-product-controller/data-product-controller (claims=flux-system/apps created=2026-08-30T14:17:34Z prune=disabled)'
expect_line '  CiliumNetworkPolicy.cilium.io data-product-controller/allow-data-product-controller (claims=flux-system/apps created=2026-08-30T14:17:34Z prune=-)'
expect_text 'Delete each one, or'
expect_no_text 'web/settings'
expect_no_text '::warning::'
expect_summary '**3 found**'
# shellcheck disable=SC2016 # literal Markdown backticks
expect_summary '`HelmRelease.helm.toolkit.fluxcd.io data-product-controller/data-product-controller`'

# The same objects, created before the merge group was built, were not left by
# its deploy: most likely a retirement awaiting its manual step. They are listed
# as a warning, and the heal succeeds.
case_name='orphans older than the merge group'
run "${dir}" FLUX_ORPHANS_SINCE=2026-08-30T15:00:00Z
expect_status 0
expect_reads 1
expect_text '::warning::3 object(s) outside every inventory predate 2026-08-30T15:00:00Z, so no deploy since then left them; they are not failed on here:'
expect_line '  Namespace data-product-controller (claims=flux-system/apps created=2026-08-30T14:17:34Z prune=disabled)'
expect_text 'prune-protected-orphan-alert CronJob'
expect_line '✅ No object is outside every inventory among Flux-applied objects created since 2026-08-30T15:00:00Z (7 Flux-applied objects, 2 Kustomizations; aggregated API groups not read: spdx.softwarecomposition.kubescape.io).'
expect_summary '- Flux orphaned objects older than 2026-08-30T15:00:00Z, not failed on:'

# An object created at the very second the group was built is residue, and a
# merge-group timestamp written with +00:00 is the same instant.
case_name='created at the boundary'
run "${dir}" FLUX_ORPHANS_SINCE=2026-08-30T14:17:34+00:00
expect_status 1
expect_text 'created since 2026-08-30T14:17:34Z'

# Without a boundary every orphan fails the check.
case_name='no boundary'
run "${dir}"
expect_status 1
expect_text "::error::3 object(s) among all Flux-applied objects are in no Kustomization's inventory"

case_name='boundary with a non-UTC offset'
run "${dir}" FLUX_ORPHANS_SINCE=2026-08-30T16:17:34+02:00
expect_status 2
expect_text "::error::FLUX_ORPHANS_SINCE '2026-08-30T16:17:34+02:00' is not a UTC timestamp."

# Controllers copy a Service's or Certificate's labels onto what they derive
# from it. None of those carries kustomize-controller's field manager, so none
# is Flux's to track. An API that ignores the label selector returns unlabelled
# objects, which are not Flux's either. An object handed to another controller
# on purpose, or already being deleted, is not reported.
case_name='derived, handed-over and deleting objects'
dir="$(scenario not-reported)"
{
  obj v1 Endpoints web web uid-ep flux-system/apps kube-controller-manager
  obj discovery.k8s.io/v1 EndpointSlice web web-abcde uid-eps flux-system/apps kube-controller-manager
  obj acme.cert-manager.io/v1 Order web web-1-123 uid-order flux-system/apps cert-manager-orders
  obj v1 ConfigMap web unlabelled uid-unlabelled - kustomize-controller
  obj v1 Namespace - tenant-a uid-adopted flux-system/apps kustomize-controller \
    '{"annotations":{"kustomize.toolkit.fluxcd.io/prune":"disabled","platform.devantler.tech/prune-orphan":"adopted"}}'
  obj v1 ConfigMap web owned uid-owned flux-system/apps kustomize-controller \
    '{"ownerReferences":[{"apiVersion":"kro.run/v1alpha1","kind":"Tenant","name":"a","uid":"x"}]}'
  obj v1 Namespace - leaving uid-leaving flux-system/apps kustomize-controller \
    '{"deletionTimestamp":"2026-08-30T15:00:00Z"}'
} | add_objects "${dir}"
run "${dir}"
expect_status 0
expect_reads 1
expect_text '(7 Flux-applied objects, 2 Kustomizations;'

# Flux transcodes colons only for the four RBAC kinds, exactly as its inventory does.
case_name='colon transcoding is RBAC only'
dir="$(scenario rbac-only)"
obj cilium.io/v2 CiliumNetworkPolicy web a:b uid-colon flux-system/apps kustomize-controller | add_objects "${dir}"
record "${dir}/kustomizations.json" 'web_a__b_cilium.io_CiliumNetworkPolicy'
run "${dir}"
expect_status 1
expect_line '  CiliumNetworkPolicy.cilium.io web/a:b (claims=flux-system/apps created=2026-08-30T14:17:34Z prune=-)'

# An object the first read catches mid-reconcile, before its Kustomization
# records the new inventory, is in the inventory by the second read.
case_name='reconcile in progress'
dir="$(scenario in-progress)"
obj v1 ConfigMap web fresh uid-fresh flux-system/apps kustomize-controller | add_objects "${dir}"
cp "${dir}/kustomizations.json" "${dir}/kustomizations.2.json"
record "${dir}/kustomizations.2.json" 'web_fresh__ConfigMap'
run "${dir}"
expect_status 0
expect_reads 2
expect_text '✅ No object stayed outside every inventory across both reads among all Flux-applied objects: the 1 found on the first read were in an inventory by the second'
expect_summary '(1 cleared on re-read)'

# An object that first appears outside every inventory on the second read is
# not confirmed; it is reported, but it does not fail the check.
case_name='finding only on the second read'
dir="$(scenario second-only)"
obj v1 ConfigMap web early uid-early flux-system/apps kustomize-controller | add_objects "${dir}"
cp "${dir}/kustomizations.json" "${dir}/kustomizations.2.json"
record "${dir}/kustomizations.2.json" 'web_early__ConfigMap'
obj v1 ConfigMap web late uid-late flux-system/apps kustomize-controller | add_objects "${dir}" 2
run "${dir}"
expect_status 0
expect_text 'not confirmed'
expect_line '  ConfigMap web/late (claims=flux-system/apps created=2026-08-30T14:17:34Z prune=-)'

# An object claimed by a Kustomization that no longer exists is an orphan.
case_name='claiming Kustomization deleted'
dir="$(scenario deleted-ks)"
obj v1 ConfigMap web left uid-left web/web-app kustomize-controller | add_objects "${dir}"
run "${dir}"
expect_status 1
expect_line '  ConfigMap web/left (claims=web/web-app created=2026-08-30T14:17:34Z prune=-)'

# A Kustomization that has recorded no inventory, or is not Ready, may simply not
# have finished an apply, so what it claims cannot be judged: UNKNOWN, never clean
# and never an orphan.
case_name='Kustomization without an inventory'
dir="$(scenario no-inventory)"
obj v1 ConfigMap tenant cfg uid-tenant tenant/tenant kustomize-controller | add_objects "${dir}"
ks tenant tenant True none | add_kustomizations "${dir}/kustomizations.json"
run "${dir}"
expect_status 2
expect_text 'is not Ready or has recorded no inventory, so they cannot be judged:'
expect_line '  ConfigMap tenant/cfg (claims=tenant/tenant created=2026-08-30T14:17:34Z prune=- reason=no-inventory)'
expect_text '::error::Could not tell whether Flux left objects outside every inventory: a Kustomization that claims objects outside every inventory is not Ready or has no inventory.'
# The reads succeeded, so their warnings are not offered as the reason.
expect_no_text 'deprecated'

case_name='Kustomization not Ready'
dir="$(scenario not-ready)"
obj v1 ConfigMap tenant cfg uid-tenant tenant/tenant kustomize-controller | add_objects "${dir}"
ks tenant tenant False tenant_other__ConfigMap | add_kustomizations "${dir}/kustomizations.json"
run "${dir}" FLUX_ORPHANS_SINCE=2026-08-30T14:05:12Z
expect_status 2
expect_line '  ConfigMap tenant/cfg (claims=tenant/tenant created=2026-08-30T14:17:34Z prune=- reason=not-ready)'

# An orphan still fails the check beside an object that cannot be judged.
case_name='orphan beside an unjudgeable object'
obj v1 ConfigMap web stray uid-stray flux-system/apps kustomize-controller | add_objects "${dir}"
run "${dir}"
expect_status 1
expect_line '  ConfigMap web/stray (claims=flux-system/apps created=2026-08-30T14:17:34Z prune=-)'
expect_text 'These objects could not be judged'

# A read that fails every attempt is UNKNOWN, and says why.
case_name='cluster unreadable'
dir="$(scenario unreadable)"
run "${dir}" FAKE_FAIL_READS=1,2,3
expect_status 2
expect_reads 3
expect_text '::error::Could not tell whether Flux left objects outside every inventory: the cluster could not be read.'
expect_text 'The connection to the server was refused'
expect_summary '- Flux orphaned objects: UNKNOWN'

# A transient failure is retried.
case_name='transient read failure'
dir="$(scenario transient)"
run "${dir}" FAKE_FAIL_READS=1
expect_status 0
expect_reads 2
expect_text 'Read 1/3 failed; retrying in 0s.'
expect_text '✅ No object is outside every inventory'

# The second read is retried and can fail too.
case_name='second read unreadable'
dir="$(scenario second-unreadable)"
obj v1 ConfigMap web stray uid-stray flux-system/apps kustomize-controller | add_objects "${dir}"
run "${dir}" FAKE_FAIL_READS=2,3,4
expect_status 2
expect_text 'the cluster could not be read a second time.'

# A read that found no Kustomization, or no Flux-applied object, examined
# nothing and can never report a clean cluster.
case_name='no Kustomizations'
dir="$(scenario no-kustomizations)"
printf '{"items":[]}\n' >"${dir}/kustomizations.json"
run "${dir}"
expect_status 2
expect_text 'the first read examined nothing.'
expect_text 'the read found 0 Kustomization(s)'

case_name='no Flux-applied objects'
dir="$(scenario no-objects)"
obj v1 Endpoints web web uid-ep flux-system/apps kube-controller-manager | items "${dir}/objects.json"
run "${dir}"
expect_status 2
expect_text 'the first read examined nothing.'
expect_text '0 Flux-applied object(s)'

case_name='settle interval validated'
dir="$(scenario bad-settle)"
run "${dir}" FLUX_ORPHANS_SETTLE_SECONDS=soon
expect_status 2
expect_text 'must be whole numbers of seconds'

echo 'check-flux-orphaned-objects: all cases passed'
