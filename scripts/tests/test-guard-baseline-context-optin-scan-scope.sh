#!/usr/bin/env bash
#
# Cover guard-baseline-context-optin-scan-scope.sh.
#
# The load-bearing case is "opted in, inside the unscanned set, caveat stated somewhere
# else" — the exact shape the tree had before #3925, where the caveat lived in
# helm-release.yaml about a different change. A guard that accepts a mention anywhere in
# the repository passes on that tree, so the fixture removes the declaration from the
# opt-in file only and asserts the guard still fails.
#
# The over-firing direction is asserted too: `velero` is opted in and IS scanned, so a
# guard that keys on the label alone rather than on the intersection would fail a
# correct tree, and the obvious way to silence that is to add a caveat that is untrue.
#
# Every fail-closed claim is proven by ablation, and each bad input is built from the
# REAL files and mutated ONE way, so no case can pass because its fixture failed to
# build. Each mutation is asserted to have landed before the guard is run on it.

set -euo pipefail

cd "$(dirname "$0")/../.."
readonly GUARD='scripts/guard-baseline-context-optin-scan-scope.sh'
readonly REAL_HR='k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml'
readonly REAL_NS='k8s/bases/infrastructure/controllers/kubescape/namespace.yaml'
readonly REAL_VELERO='k8s/bases/infrastructure/controllers/velero/namespace.yaml'
readonly MARKER='kubescape-scan-scope: excluded'

tmp="$(mktemp -d)"
# A test that exits 0 without having run is worse than one that errors. Bash 3.2 reports
# $? as 0 to an EXIT trap for a `set -u` abort, and a successful `rm` in the trap can
# become the script's own status. Completion is therefore recorded explicitly: reaching
# the end is the only way a zero status leaves this script.
finished=0
cleanup() {
  local rc=$?
  rm -rf "${tmp}"
  if [ "${finished}" != 1 ] && [ "${rc}" -eq 0 ]; then
    printf 'test-guard-baseline-context-optin-scan-scope: aborted before finishing; reporting failure rather than a clean pass\n' >&2
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

for f in "${REAL_HR}" "${REAL_NS}" "${REAL_VELERO}"; do
  [ -f "${f}" ] || {
    printf 'FAIL: fixture source %s is missing; every case below would be vacuous\n' "${f}" >&2
    exit 1
  }
done

# Build a fixture tree from the REAL files. Callers mutate exactly one thing afterwards.
mkfixture() { # <name> -> prints the fixture root
  local name="$1"
  local root="${tmp}/${name}"
  mkdir -p "${root}/k8s/kubescape" "${root}/k8s/velero"
  cp "${REAL_HR}" "${root}/helm-release.yaml"
  cp "${REAL_NS}" "${root}/k8s/kubescape/namespace.yaml"
  cp "${REAL_VELERO}" "${root}/k8s/velero/namespace.yaml"
  printf '%s\n' "${root}"
}

run() { # <helm-release> <k8s-dir> -> prints the exit status
  local rc=0
  bash "${GUARD}" "$1" "$2" >/dev/null 2>&1 || rc=$?
  printf '%s\n' "${rc}"
}
expect() { # <label> <expected-rc> <helm-release> <k8s-dir>
  local label="$1" want="$2" got
  got="$(run "$3" "$4")"
  if [ "${got}" = "${want}" ]; then ok "${label} — exit ${got}"; else
    bad "${label} — expected exit ${want}, got ${got}"
  fi
}

# --- the tree as it stands ------------------------------------------------------------
rc=0
bash "${GUARD}" >/dev/null 2>&1 || rc=$?
if [ "${rc}" = 0 ]; then ok 'the repository as it stands passes'; else
  bad "the repository as it stands should pass, got exit ${rc}"
fi

# --- AC1/AC2: THE DEFECT — opted in, unscanned, caveat not at the opt-in ---------------
root="$(mkfixture undeclared)"
ns="${root}/k8s/kubescape/namespace.yaml"
grep -qF -- "${MARKER}" "${ns}" || bad 'fixture build: the real opt-in file carries no marker to remove'
grep -vF -- "${MARKER}" "${ns}" >"${ns}.new" && mv "${ns}.new" "${ns}"
grep -qF -- "${MARKER}" "${ns}" && bad 'fixture build: the marker survived removal'
# The caveat still exists in helm-release.yaml, untouched — the pre-#3925 shape exactly.
expect 'an unscanned opt-in whose caveat is only in another file fails' 1 "${root}/helm-release.yaml" "${root}/k8s"

guard_out="$(bash "${GUARD}" "${root}/helm-release.yaml" "${root}/k8s" 2>&1 || true)"
if grep -q 'kubescape' <<<"${guard_out}"; then
  ok 'the failure names the offending namespace'
else
  bad 'the failure did not name kubescape'
fi
if grep -qF -- "${MARKER}" <<<"${guard_out}"; then
  ok 'the failure names the exact line to add'
else
  bad 'the failure did not name the marker to add'
fi
if grep -qi 'do not exempt' <<<"${guard_out}"; then
  ok 'the failure steers away from exempting the namespace'
else
  bad 'the failure did not warn against exempting instead of declaring'
fi

# --- the over-firing direction: a scanned opt-in needs no caveat -----------------------
root="$(mkfixture scanned-optin)"
grep -qF -- "${MARKER}" "${root}/k8s/velero/namespace.yaml" &&
  bad 'fixture build: velero unexpectedly already carries the marker'
expect 'a scanned opted-in namespace without the caveat passes' 0 \
  "${root}/helm-release.yaml" "${root}/k8s"

# --- drift the other way: a scanned namespace later becomes unscanned ------------------
root="$(mkfixture newly-excluded)"
hr="${root}/helm-release.yaml"
sed -i.bak 's/^\( *excludeNamespaces: "\)kubescape,/\1kubescape,velero,/' "${hr}" && rm -f "${hr}.bak"
yq -r '.spec.values.excludeNamespaces' "${hr}" | grep -q 'velero' ||
  bad 'fixture build: velero was not added to excludeNamespaces'
expect 'a namespace added to excludeNamespaces while opted in fails' 1 "${hr}" "${root}/k8s"

# --- the same opt-in written with the other YAML suffix ---------------------------------
# k8s/ is all .yaml today, but the repository uses .yml elsewhere, so a sweep pinned to one
# suffix would not FAIL on such a file — it would not see it at all, which is the silent
# miss this case exists to keep closed.
root="$(mkfixture yml-suffix)"
ns="${root}/k8s/kubescape/namespace.yaml"
grep -vF -- "${MARKER}" "${ns}" >"${root}/k8s/kubescape/namespace.yml"
rm -f "${ns}"
[ -f "${root}/k8s/kubescape/namespace.yml" ] || bad 'fixture build: the .yml namespace was not created'
grep -qF -- "${MARKER}" "${root}/k8s/kubescape/namespace.yml" &&
  bad 'fixture build: the marker survived removal in the .yml file'
expect 'an undeclared opt-in in a .yml manifest is still caught' 1 \
  "${root}/helm-release.yaml" "${root}/k8s"

# --- AC3: fail closed on every unreadable or empty input -------------------------------
root="$(mkfixture empty-exclusions)"
hr="${root}/helm-release.yaml"
sed -i.bak 's/^\( *excludeNamespaces: \).*/\1""/' "${hr}" && rm -f "${hr}.bak"
[ -z "$(yq -r '.spec.values.excludeNamespaces' "${hr}")" ] ||
  bad 'fixture build: excludeNamespaces was not emptied'
expect 'an empty excludeNamespaces is cannot-check, not a pass' 2 "${hr}" "${root}/k8s"

root="$(mkfixture moved-path)"
hr="${root}/helm-release.yaml"
sed -i.bak 's/^\( *\)excludeNamespaces:/\1excludeNamespacesRenamed:/' "${hr}" && rm -f "${hr}.bak"
yq -r '.spec.values.excludeNamespaces' "${hr}" | grep -qx 'null' ||
  bad 'fixture build: the excludeNamespaces key was not renamed'
expect 'a moved excludeNamespaces path is cannot-check' 2 "${hr}" "${root}/k8s"

root="$(mkfixture no-optin)"
rm -rf "${root}/k8s/kubescape" "${root}/k8s/velero"
! grep -rqF 'baseline-context' "${root}/k8s" 2>/dev/null ||
  bad 'fixture build: an opted-in namespace survived removal'
expect 'no opted-in namespace at all is cannot-check, not a clean tree' 2 \
  "${root}/helm-release.yaml" "${root}/k8s"

root="$(mkfixture missing-inputs)"
expect 'a missing HelmRelease is cannot-check' 2 "${root}/nope.yaml" "${root}/k8s"
expect 'a missing k8s directory is cannot-check' 2 "${root}/helm-release.yaml" "${root}/nope"

root="$(mkfixture unparseable)"
printf '\n\tthis: is: not: yaml\n  - [\n' >>"${root}/helm-release.yaml"
yq -r '.spec.values.excludeNamespaces' "${root}/helm-release.yaml" >/dev/null 2>&1 &&
  bad 'fixture build: the HelmRelease still parses'
expect 'an unparseable HelmRelease is cannot-check' 2 "${root}/helm-release.yaml" "${root}/k8s"

# --- result ----------------------------------------------------------------------------
if [ "${failures}" -ne 0 ]; then
  printf '\n%d case(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall cases passed\n'
finished=1
