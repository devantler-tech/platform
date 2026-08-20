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
# does author. A few exact check:path pairs on those roles now carry their own reviewed
# dispositions (#2990), listed literally below and guarded by their own premises test; every other
# check:path combination stays live.
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

# First-party RBAC dispositions for these same check ids (#2990), as exact CHECK:PATH pairs.
# Pairs, not bare paths: a path-only allow-list would let ANY of these five checks be suppressed on
# a reviewed role — KSV-0053 (pods/exec) on the tenant role, say — which is a widening this test
# exists to catch. Each pair below is one reviewed verdict, and anything else fails as a widened
# skip. Their premises are guarded separately by
# scripts/tests/test-trivyignore-first-party-rbac-boundary.sh, which also rejects any pair not
# listed here so the two files cannot drift apart.
readonly first_party_pairs=(
  'KSV-0041:k8s/bases/infrastructure/cluster-roles/tenant-base-edit.yaml'
  'KSV-0056:k8s/bases/infrastructure/cluster-roles/tenant-base-edit.yaml'
  'KSV-0056:k8s/bases/infrastructure/resource-graph-definitions/tenant/cluster-role.yaml'
  'KSV-0056:k8s/bases/infrastructure/resource-graph-definitions/webapp/cluster-role.yaml'
  'KSV-0046:k8s/bases/infrastructure/cluster-roles/cluster-reader.yaml'
)

# Reviewed NON-RBAC dispositions for these same check ids, as exact CHECK:PATH pairs. Kept
# separate from first_party_pairs above because it is a different KIND of verdict: those turn
# on how a ClusterRole is BOUND, these on a container identity baked into an upstream image.
# Folding them together would let an RBAC premises test vouch for a workload-identity claim it
# never checks. Each pair here is guarded by its own premises test, named beside it.
readonly reviewed_workload_pairs=(
  # openbao runs at its image-baked uid 100; guarded per container by
  # scripts/tests/test-trivyignore-vault-config-identity-boundary.sh (#2787).
  'KSV-0020:k8s/bases/infrastructure/vault-config/job.yaml'
  # openbao's image-baked gid 1000, the same identity one field over; guarded per container by
  # the same test, which asserts both dispositions together (#3258).
  'KSV-0021:k8s/bases/infrastructure/vault-config/job.yaml'
)

# THIRD reviewed category: an ADMISSION verdict — the suppressed field is not absent at runtime, it
# is supplied by a namespace-scoped object the scanner cannot see because it reads one file at a
# time. Kept separate from both lists above for the same reason those are separate from each other:
# an RBAC premises test and a workload-identity premises test each check something this claim is not
# about, so folding it in would let either vouch for a premise it never examines.
#
# Its premises test is scripts/guard-limitrange-premise.sh, which walks the kustomize graph of every
# provider overlay and requires the one shipping the file to also ship a Container-type
# `default.cpu` LimitRange into that namespace. It reports `ok ... provider docker ships a
# CPU-defaulting LimitRange into kube-system` for this pair today.
#
# That guard matches on `checkov.io/skipN` annotation values, so on its own it vouches for the
# file's CKV_K8S_11 skip rather than for the trivy entry below. The admission-premise section near
# the end of this file binds the two by requiring a passing verdict naming this pair's own file, so
# removing the annotation while keeping the entry fails the build instead of quietly leaving the
# pair unpremised.
readonly reviewed_admission_pairs=(
  # The kube-system default-limitrange supplies the CPU limit at admission; trivy reads the
  # committed spec, where an admission-time default is absent (#2787).
  'KSV-0011:k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml'
)

# Every check id dispositioned for the vendored bundles. Keep in sync with .trivyignore.yaml.
readonly checks=(KSV-0011 KSV-0014 KSV-0018 KSV-0020 KSV-0021 KSV-0041 KSV-0046 KSV-0053 KSV-0056 KSV-0114)

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
        # Three reviewed categories, matched by three SIBLING loops. Nesting the workload and
        # admission scans inside the first-party loop couples them to that list being non-empty:
        # with no first-party pairs the outer body never runs, so every workload and admission
        # pair reads as unreviewed and a correct repository fails the build.
        for fp in ${first_party_pairs[@]+"${first_party_pairs[@]}"}; do
          if [ "$check:$path" = "$fp" ]; then
            reviewed=1
            break
          fi
        done
        # Second reviewed category: a workload-identity verdict, guarded by its own premises
        # test rather than by the RBAC one above.
        if [ "$reviewed" -eq 0 ]; then
          for wp in ${reviewed_workload_pairs[@]+"${reviewed_workload_pairs[@]}"}; do
            if [ "$check:$path" = "$wp" ]; then
              reviewed=1
              break
            fi
          done
        fi
        # Third reviewed category: an admission verdict, whose premise the admission-premise
        # section below checks directly against this pair's own file.
        if [ "$reviewed" -eq 0 ]; then
          for ap in ${reviewed_admission_pairs[@]+"${reviewed_admission_pairs[@]}"}; do
            if [ "$check:$path" = "$ap" ]; then
              reviewed=1
              break
            fi
          done
        fi
        if [ "$reviewed" -eq 0 ]; then
          printf 'WIDENED SKIP: %s is scoped to %s, which is neither a vendored operator bundle nor a reviewed first-party, workload-identity or admission disposition for THAT check\n' "$check" "$path" >&2
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

# ------------------------------------------------------------------ premise --
# The KSV-0014 disposition rests on a claim the structural layer cannot see: these two Deployments
# do not run in production. That is what makes a writable root filesystem on them a local-test-bed
# concern, so if the claim stops holding the exception must stop holding with it.
#
# As committed, the VM stack ships NOWHERE: the base controller aggregate excludes it, and the only
# overlay that carries it at all — the docker (local/CI) provider — keeps both entries commented out
# as an opt-in. Confirmed against the live production cluster on 2026-08-19: zero cdi/kubevirt pods
# and zero such Deployments, CRDs only.
#
# What is asserted here is the boundary rather than the current count: no kustomization OUTSIDE
# k8s/providers/docker/ may actively include the VM stack. Uncommenting it in the docker test-bed
# stays allowed and changes nothing; adding it to the base aggregate or any production overlay fails
# the build instead of silently keeping an exception whose reason has expired.
#
# Comments are stripped before matching, so the aggregate's own exclusion note is not a reference.
# Kustomize accepts a directory entry with or without a trailing slash, so both spellings must
# match, and a reference INTO the directory (…/cdi/cdi-operator.yaml) must keep matching too.
readonly vm_entry='^[[:space:]]*-[[:space:]]+(.*/)?(cdi|kubevirt)(/|[[:space:]]*(#.*)?$)'
# A directory every overlay really does include, used to prove the matcher works. Without it, a
# broken search reports zero VM references and this whole control passes vacuously.
readonly control_entry='^[[:space:]]*-[[:space:]]+(.*/)?cilium/'

matching_kustomizations() {
  local pattern="$1" k
  while IFS= read -r k; do
    grep -vE '^[[:space:]]*#' "$k" | grep -qE "$pattern" && printf '%s\n' "$k"
  done < <(cd "$repo_root" && find k8s -name kustomization.yaml | sort)
  return 0
}

control_hits="$(cd "$repo_root" && matching_kustomizations "$control_entry" | wc -l | tr -d ' ')"
if [ "${control_hits:-0}" -lt 1 ]; then
  printf 'PREMISE UNCHECKABLE: the kustomization matcher found no reference to a directory known to be included, so a zero VM-stack result would be meaningless\n' >&2
  status=1
else
  while IFS= read -r ref; do
    [ -n "$ref" ] || continue
    case "$ref" in
      k8s/providers/docker/*) ;;
      *)
        printf 'PREMISE BROKEN: %s actively includes the VM stack from outside the docker provider, so these Deployments can reach production and the KSV-0014 disposition no longer holds\n' "$ref" >&2
        status=1
        ;;
    esac
  done < <(cd "$repo_root" && matching_kustomizations "$vm_entry")
fi

# ------------------------------------------------------ admission premise --
# Every reviewed_admission_pairs entry rests on "a namespace-scoped LimitRange supplies this field
# at admission, and trivy reading one stored file cannot see it". scripts/guard-limitrange-premise.sh
# is what checks that claim against the kustomize graph, but it matches on `checkov.io/skipN`
# annotation values, so by itself it vouches for a file's CHECKOV skip and never for the trivy entry
# beside it. Requiring a passing verdict naming the pair's own file is what binds the two: remove the
# annotation while keeping the trivy entry and the guard stops reporting that file, which fails here
# instead of leaving the entry resting on a premise nothing checks.
premise_guard="$repo_root/scripts/guard-limitrange-premise.sh"
if [ ! -x "$premise_guard" ]; then
  printf 'ADMISSION PREMISE UNCHECKABLE: %s is missing or not executable\n' "$premise_guard" >&2
  status=1
else
  # Capture the guard's status DIRECTLY rather than reading $? after an `if` compound, which
  # yields the compound's status (0 when the condition merely failed) and would collapse
  # "could not check" into "checked".
  premise_out="$(cd "$repo_root" && "$premise_guard" k8s 2>&1)"
  premise_rc=$?
  if [ "$premise_rc" -eq 2 ]; then
    printf 'ADMISSION PREMISE UNCHECKABLE: guard-limitrange-premise.sh exited 2, so an absent verdict below would mean nothing rather than a broken premise\n' >&2
    printf '%s\n' "$premise_out" >&2
    status=1
  else
    for ap in ${reviewed_admission_pairs[@]+"${reviewed_admission_pairs[@]}"}; do
      ap_path="${ap#*:}"
      # The guard prints `ok   <file> — provider <name> ships …`. The trailing space is load-bearing:
      # without it a path that is a prefix of a longer reported one would satisfy this.
      printf '%s\n' "$premise_out" | grep -qF -- "ok   $ap_path " && continue
      printf 'UNPREMISED ADMISSION SKIP: %s is dispositioned as an admission verdict, but guard-limitrange-premise.sh reports no passing LimitRange premise for %s, so the trivy entry rests on a premise nothing checks\n' "$ap" "$ap_path" >&2
      status=1
    done
  fi
fi

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
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: boundary-probe
spec:
  selector:
    matchLabels: { app: boundary-probe }
  template:
    metadata:
      labels: { app: boundary-probe }
    spec:
      containers:
        - name: boundary-probe
          image: busybox:1.36
          securityContext:
            allowPrivilegeEscalation: false
            runAsNonRoot: true
            capabilities:
              drop: ["ALL"]
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

printf 'vendored-operator RBAC disposition is path-scoped: %d checks suppressed on both vendored bundles, still live on every first-party role but the %d reviewed check:path exceptions\n' "${#checks[@]}" "${#first_party_pairs[@]}"
