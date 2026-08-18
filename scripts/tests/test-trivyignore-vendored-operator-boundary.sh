#!/usr/bin/env bash
# The vendored-operator RBAC dispositions in .trivyignore.yaml are PATH-SCOPED, and this test is
# what keeps them that way.
#
# WHY THIS EXISTS
# KubeVirt's and CDI's install bundles are upstream release artifacts, vendored verbatim and
# SHA-256 verified by scripts/update-vendored-operators.sh. Their ClusterRoles grant wildcard,
# secret, webhook, exec and networking permissions because the operators require them, and those
# grants are not ours to narrow — see the disposition block in .trivyignore.yaml and #2990.
#
# The risk that disposition creates is NOT that it is wrong; it is that it silently stops being
# path-scoped. Drop a `paths:` key, or widen one to k8s/**, and every one of these checks goes
# quiet across the whole repository — including on the FIRST-PARTY cluster roles this repository
# does author. Four of those roles now carry their own reviewed dispositions (#2990), listed
# literally below and guarded by their own premises test; the checks stay live on every other role.
# Nothing else would fail: the scan still runs, the count still drops, and the gate still reports
# a smaller number, which reads exactly like progress.
#
# Verified here in two layers, because they fail for different reasons:
#
#   STRUCTURE (always) — every dispositioned check id is scoped to EXACTLY the two vendored bundles,
#   written literally: no entry may lose its `paths:` key, gain a path outside them, or drop one of
#   them. It needs nothing but yq, so it is the layer that still holds when the behavioural control
#   below is skipped.
#
#   BEHAVIOUR (when trivy is installed) — the paired control the KSV-0037 entry describes: the same
#   offending ClusterRole bytes are written to a vendored path AND to a first-party path, and the
#   suppression must apply to exactly the first. This proves trivy agrees with the structure rather
#   than assuming it. It is skipped, loudly, where trivy is not on PATH.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly ignorefile="$repo_root/.trivyignore.yaml"

command -v yq >/dev/null 2>&1 || {
  printf 'yq is required to check the vendored-operator disposition boundary\n' >&2
  exit 1
}

readonly vendored_cdi='k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml'
readonly vendored_kubevirt='k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml'
readonly first_party='k8s/bases/infrastructure/cluster-roles/boundary-probe.yaml'

# First-party RBAC paths that carry their OWN reviewed disposition for one of these same check ids
# (#2990). They are listed literally, so this stays fail-closed: any path that is neither a vendored
# bundle nor one of these fails as a widened skip, exactly as before. Their premises are guarded
# separately by scripts/tests/test-trivyignore-first-party-rbac-boundary.sh.
readonly first_party_dispositioned=(
  'k8s/bases/infrastructure/cluster-roles/tenant-base-edit.yaml'
  'k8s/bases/infrastructure/cluster-roles/cluster-reader.yaml'
  'k8s/bases/infrastructure/resource-graph-definitions/tenant/cluster-role.yaml'
  'k8s/bases/infrastructure/resource-graph-definitions/webapp/cluster-role.yaml'
)

# Every check id dispositioned for the vendored bundles. Keep in sync with .trivyignore.yaml.
readonly checks=(KSV-0041 KSV-0046 KSV-0053 KSV-0056 KSV-0114)

status=0

# ---------------------------------------------------------------- structure --
for check in "${checks[@]}"; do
  # A yq failure here must take the MISSING DISPOSITION path below, not become an
  # integer-expression error that evaluates false and silently treats the id as present.
  entries="$(yq "[.misconfigurations[] | select(.id == \"$check\")] | length" "$ignorefile" 2>/dev/null || printf '0')"
  entries="${entries:-0}"
  if [ "$entries" -lt 1 ]; then
    printf 'MISSING DISPOSITION: %s has no entry in .trivyignore.yaml\n' "$check" >&2
    status=1
    continue
  fi

  # One line per entry, so an entry that lost its paths key cannot hide behind one that kept it.
  while IFS= read -r n; do
    if [ "$n" -eq 0 ]; then
      printf 'UNSCOPED SKIP: an entry for %s has no paths, which suppresses it everywhere including first-party cluster roles\n' "$check" >&2
      status=1
    fi
  done < <(yq ".misconfigurations[] | select(.id == \"$check\") | (.paths // []) | length" "$ignorefile")

  seen_cdi=0
  seen_kubevirt=0
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    case "$path" in
      "$vendored_cdi") seen_cdi=1 ;;
      "$vendored_kubevirt") seen_kubevirt=1 ;;
      *)
        reviewed=0
        for fp in "${first_party_dispositioned[@]}"; do
          if [ "$path" = "$fp" ]; then
            reviewed=1
            break
          fi
        done
        if [ "$reviewed" -eq 0 ]; then
          printf 'WIDENED SKIP: %s is scoped to %s, which is neither a vendored operator bundle nor a reviewed first-party disposition\n' "$check" "$path" >&2
          status=1
        fi
        ;;
    esac
  done < <(yq ".misconfigurations[] | select(.id == \"$check\") | (.paths // [])[]" "$ignorefile")

  # Both bundles, or the disposition is no longer the one that was reviewed. Without this the
  # structural layer accepts a dropped path, and the behavioural half that would notice is skipped
  # wherever trivy or jq is absent.
  [ "$seen_cdi" -eq 1 ] || {
    printf 'NARROWED SKIP: %s is no longer scoped to %s\n' "$check" "$vendored_cdi" >&2
    status=1
  }
  [ "$seen_kubevirt" -eq 1 ] || {
    printf 'NARROWED SKIP: %s is no longer scoped to %s\n' "$check" "$vendored_kubevirt" >&2
    status=1
  }
done

# ---------------------------------------------------------------- behaviour --
if ! command -v trivy >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  printf 'NOTE: trivy or jq not on PATH; structural checks ran, paired control skipped\n'
  [ "$status" -eq 0 ] || exit 1
  printf 'vendored-operator RBAC disposition is path-scoped (structure only): %d checks\n' "${#checks[@]}"
  exit 0
fi

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

# One ClusterRole that trips all five: wildcard resources, secrets, webhook configurations,
# pods/exec and service/endpoint management.
write_probe() {
  local dest="$scratch/$1"
  mkdir -p "$(dirname "$dest")"
  cat >"$dest" <<'PROBE'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: boundary-probe
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["*"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "create", "update", "delete"]
  - apiGroups: ["admissionregistration.k8s.io"]
    resources: ["mutatingwebhookconfigurations", "validatingwebhookconfigurations"]
    verbs: ["get", "list", "create", "update", "delete"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["services", "endpoints"]
    verbs: ["get", "list", "create", "update", "delete"]
PROBE
}

write_probe "$vendored_cdi"
write_probe "$vendored_kubevirt"
write_probe "$first_party"
cp "$ignorefile" "$scratch/.trivyignore.yaml"

findings="$scratch/findings.txt"
trivy_json="$scratch/trivy.json"
trivy_err="$scratch/trivy.err"

# trivy's stderr is kept rather than discarded: `set -o pipefail` means a trivy failure would
# otherwise exit this script non-zero with no diagnostic, which is the "guardrail that blocks
# without naming the fix" shape. Run it on its own so its status is checked directly.
if ! (cd "$scratch" && trivy fs --scanners misconfig --exit-code 0 \
  --ignorefile .trivyignore.yaml --format json --quiet . >"$trivy_json" 2>"$trivy_err"); then
  printf 'trivy failed while scanning the probe fixture; its stderr follows\n' >&2
  cat "$trivy_err" >&2
  exit 1
fi

jq -r '.Results[]? as $r | $r.Misconfigurations[]?
       | select(.Status=="FAIL") | "\($r.Target)\t\(.ID)"' "$trivy_json" |
  sed 's|^\./||' | sort -u >"$findings"

[ -s "$findings" ] || {
  printf 'probe produced no findings at all: fixture or scan is broken, so this control would pass vacuously\n' >&2
  exit 1
}

fires() { grep -qxF -- "$1	$2" "$findings"; }

# The control half. Without it, a blanket suppression satisfies every assertion below it.
for check in "${checks[@]}"; do
  fires "$first_party" "$check" || {
    printf 'BOUNDARY LOST: %s no longer fires on a first-party cluster role; the disposition is hiding real findings\n' "$check" >&2
    status=1
  }
done

for path in "$vendored_cdi" "$vendored_kubevirt"; do
  for check in "${checks[@]}"; do
    if fires "$path" "$check"; then
      printf 'MISSING DISPOSITION: %s still fires on vendored bundle %s\n' "$check" "$path" >&2
      status=1
    fi
  done
done

[ "$status" -eq 0 ] || exit 1

printf 'vendored-operator RBAC disposition is path-scoped: %d checks suppressed on both vendored bundles, still live on every first-party role but the %d reviewed exceptions\n' "${#checks[@]}" "${#first_party_dispositioned[@]}"
