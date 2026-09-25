#!/usr/bin/env bash
#
# Cover guard-kubescape-scan-scope.sh.
#
# The load-bearing case is a namespace added to excludeNamespaces with no reviewed row: that
# is the silent narrowing of scan coverage the guard exists to catch (#3215). The opposite
# direction, a row for a namespace that is scanned again, is asserted too.
#
# Every bad input is built from the REAL files and changed ONE way, and each change is
# asserted to have landed before the guard runs, so no case can pass because its fixture
# failed to build.

set -euo pipefail

cd "$(dirname "$0")/../.."
readonly GUARD='scripts/guard-kubescape-scan-scope.sh'
readonly REAL_HR='k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml'
readonly REAL_TSV='scripts/kubescape-unscanned-namespaces.tsv'

tmp="$(mktemp -d)"
# A test that exits 0 without having run is worse than one that errors. Bash 3.2 reports
# $? as 0 to an EXIT trap for a `set -u` abort, so completion is recorded explicitly.
finished=0
cleanup() {
  local rc=$?
  rm -rf "${tmp}"
  if [ "${finished}" != 1 ] && [ "${rc}" -eq 0 ]; then
    printf 'test-guard-kubescape-scan-scope: aborted before finishing; reporting failure rather than a clean pass\n' >&2
    rc=1
  fi
  exit "${rc}"
}
trap cleanup EXIT

failures=0
ok() { printf 'ok: %s\n' "$1"; }
bad() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

for f in "${REAL_HR}" "${REAL_TSV}"; do
  [ -f "${f}" ] || {
    printf 'FAIL: fixture source %s is missing; every case below would be vacuous\n' "${f}" >&2
    exit 1
  }
done

# expect <name> <want-exit> <want-stderr-substring> <helm-release> <tsv> [<k8s-root>]
expect() {
  local name="$1" want="$2" needle="$3" hr="$4" tsv="$5" root="${6:-k8s}" rc=0
  bash "${GUARD}" "${hr}" "${tsv}" "${root}" >"${tmp}/out" 2>"${tmp}/err" || rc=$?
  if [ "${rc}" -ne "${want}" ]; then
    bad "${name}: exit ${rc}, want ${want} ($(tr '\n' ' ' <"${tmp}/err"))"
    return
  fi
  if [ -n "${needle}" ] && ! grep -qF -- "${needle}" "${tmp}/err"; then
    bad "${name}: stderr lacks '${needle}' ($(tr '\n' ' ' <"${tmp}/err"))"
    return
  fi
  ok "${name}"
}

# landed <file> <fixed-string> — assert a mutation is present before using the fixture
landed() {
  grep -qF -- "$2" "$1" || {
    printf 'FAIL: fixture %s lacks %s; the case would be vacuous\n' "$1" "$2" >&2
    exit 1
  }
}

expect "real tree passes" 0 "" "${REAL_HR}" "${REAL_TSV}"

# A namespace newly excluded, with no reviewed row.
sed 's/excludeNamespaces: "kubescape,/excludeNamespaces: "kubescape,velero,/' "${REAL_HR}" >"${tmp}/hr-extra.yaml"
landed "${tmp}/hr-extra.yaml" 'excludeNamespaces: "kubescape,velero,'
expect "unreviewed exclusion fails" 1 "velero is excluded from scanning but has no row" \
  "${tmp}/hr-extra.yaml" "${REAL_TSV}"

# A namespace scanned again, whose row was left behind.
sed 's/excludeNamespaces: "kubescape,/excludeNamespaces: "/' "${REAL_HR}" >"${tmp}/hr-fewer.yaml"
landed "${tmp}/hr-fewer.yaml" 'excludeNamespaces: "kube-system,'
expect "stale row fails" 1 "kubescape is listed as unscanned but is not in excludeNamespaces" \
  "${tmp}/hr-fewer.yaml" "${REAL_TSV}"

# A row with no reason.
sed 's/^kube-public'$'\t''.*/kube-public/' "${REAL_TSV}" >"${tmp}/no-reason.tsv"
grep -qx 'kube-public' "${tmp}/no-reason.tsv" || {
  printf 'FAIL: no-reason fixture did not land\n' >&2
  exit 1
}
expect "row without a reason fails" 1 "kube-public is listed without a reason" \
  "${REAL_HR}" "${tmp}/no-reason.tsv"

# A duplicated row.
{ cat "${REAL_TSV}"; grep '^kubeconfig	' "${REAL_TSV}"; } >"${tmp}/dup.tsv"
[ "$(grep -c '^kubeconfig	' "${tmp}/dup.tsv")" -eq 2 ] || {
  printf 'FAIL: duplicate fixture did not land\n' >&2
  exit 1
}
expect "duplicated row fails" 1 "kubeconfig is listed more than once" \
  "${REAL_HR}" "${tmp}/dup.tsv"

# Fail closed: a moved key reads as null.
sed 's/excludeNamespaces:/excludedNamespaces:/' "${REAL_HR}" >"${tmp}/hr-moved.yaml"
landed "${tmp}/hr-moved.yaml" 'excludedNamespaces:'
expect "moved key is UNKNOWN" 2 "came back EMPTY" "${tmp}/hr-moved.yaml" "${REAL_TSV}"

# Fail closed: a reviewed list with only comments.
grep '^#' "${REAL_TSV}" >"${tmp}/empty.tsv"
expect "empty reviewed list is UNKNOWN" 2 "no rows in" "${REAL_HR}" "${tmp}/empty.tsv"

# Fail closed: missing files.
expect "missing reviewed list is UNKNOWN" 2 "not found" "${REAL_HR}" "${tmp}/absent.tsv"
expect "missing HelmRelease is UNKNOWN" 2 "not found" "${tmp}/absent.yaml" "${REAL_TSV}"

# Overrides: the base is the only place the scan scope may be set. Each case copies the real
# k8s tree and adds ONE override, so the tree itself is known to pass (the first case above).
fresh_tree() {
  rm -rf "${tmp}/k8s"
  cp -R k8s "${tmp}/k8s"
}
readonly OVERLAY_DIR="${tmp}/k8s/providers/hetzner/infrastructure/controllers/kubescape/patches"

# A strategic-merge patch that narrows the scope in one provider.
fresh_tree
cat >"${OVERLAY_DIR}/narrow-scan-scope.yaml" <<'YAML'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: kubescape
  namespace: kubescape
spec:
  values:
    excludeNamespaces: "kube-system,velero"
YAML
landed "${OVERLAY_DIR}/narrow-scan-scope.yaml" 'excludeNamespaces: "kube-system,velero"'
expect "overlay patch setting excludeNamespaces fails" 1 "sets spec.values.excludeNamespaces" \
  "${REAL_HR}" "${REAL_TSV}" "${tmp}/k8s"

# A patch that adds valuesFrom, which can carry excludeNamespaces from a ConfigMap unseen.
fresh_tree
cat >"${OVERLAY_DIR}/values-from.yaml" <<'YAML'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: kubescape
  namespace: kubescape
spec:
  valuesFrom:
    - kind: ConfigMap
      name: kubescape-values
YAML
landed "${OVERLAY_DIR}/values-from.yaml" 'valuesFrom:'
expect "overlay patch adding valuesFrom fails" 1 "sets spec.valuesFrom" \
  "${REAL_HR}" "${REAL_TSV}" "${tmp}/k8s"

# A JSON6902 patch file.
fresh_tree
cat >"${OVERLAY_DIR}/json-scan-scope.yaml" <<'YAML'
- op: replace
  path: /spec/values/excludeNamespaces
  value: "kube-system"
YAML
landed "${OVERLAY_DIR}/json-scan-scope.yaml" '/spec/values/excludeNamespaces'
expect "JSON6902 op on excludeNamespaces fails" 1 "a JSON patch op targets /spec/values/excludeNamespaces" \
  "${REAL_HR}" "${REAL_TSV}" "${tmp}/k8s"

# An inline patch in a kustomization.
fresh_tree
cat >>"${tmp}/k8s/providers/docker/infrastructure/controllers/kustomization.yaml" <<'YAML'
  - target:
      kind: HelmRelease
      name: kubescape
    patch: |-
      - op: replace
        path: /spec/values/excludeNamespaces
        value: "kube-system"
YAML
landed "${tmp}/k8s/providers/docker/infrastructure/controllers/kustomization.yaml" 'path: /spec/values/excludeNamespaces'
expect "inline kustomization patch on excludeNamespaces fails" 1 "an inline patch sets excludeNamespaces" \
  "${REAL_HR}" "${REAL_TSV}" "${tmp}/k8s"

# valuesFrom on the base itself.
printf '  valuesFrom:\n    - kind: ConfigMap\n      name: kubescape-values\n' | cat "${REAL_HR}" - >"${tmp}/hr-values-from.yaml"
[ "$(yq -r '.spec.valuesFrom[0].name' "${tmp}/hr-values-from.yaml")" = "kubescape-values" ] || {
  printf 'FAIL: base valuesFrom fixture did not land under .spec\n' >&2
  exit 1
}
expect "valuesFrom on the base fails" 1 "uses spec.valuesFrom" \
  "${tmp}/hr-values-from.yaml" "${REAL_TSV}"

# Fail closed: no k8s tree to search.
expect "missing k8s tree is UNKNOWN" 2 "not found" "${REAL_HR}" "${REAL_TSV}" "${tmp}/absent-k8s"

finished=1
if [ "${failures}" -ne 0 ]; then
  printf 'test-guard-kubescape-scan-scope: %d case(s) failed\n' "${failures}" >&2
  exit 1
fi
printf 'test-guard-kubescape-scan-scope: all cases passed\n'
