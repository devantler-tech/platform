#!/usr/bin/env bash
# The KSV-0020 disposition on the vault-config Job is PATH-scoped, and this test is what keeps it
# honest.
#
# WHY THIS EXISTS
# The Job runs four containers. Two are openbao and set `runAsUser: 100`, which is that image's own
# baked identity (`openbao:x:100:1000` in its /etc/passwd, verified against the pinned digest). The
# other two — minio/mc and alpine/k8s — bake no such identity, so the Job deliberately sets
# runAsUser PER CONTAINER and leaves them on the pod's high 65532 default. The same reasoning is
# already written into this Job's resource-scoped `checkov.io/skip2` annotation.
#
# THE SCOPING GRANULARITIES DO NOT MATCH, AND THAT IS THE WHOLE RISK.
# `checkov.io/skipN` is scoped to a RESOURCE; a trivy ignorefile entry is scoped to a PATH. So the
# trivy entry silences KSV-0020 for every container in this Job, including one added later at a low
# UID for no reason at all — precisely the widening the per-container split was written to avoid.
# Nothing in .trivyignore.yaml can express "only these two containers", so it is asserted here.
#
# Nothing else would fail if that happened: the scan still runs, the count still drops, and the gate
# still reports a smaller number, which reads exactly like progress.
#
# ⚠️ This Job ships to PRODUCTION (k8s/providers/hetzner/infrastructure/ includes
# k8s/bases/infrastructure/), so the "local/CI provider only" premise that backs the vendored
# operator dispositions is NOT available here and is not borrowed. The disposition stands on the
# image-baked identity alone — which is exactly why that identity is asserted rather than trusted.
#
# Three layers, which fail for different reasons:
#
#   STRUCTURE (always) — the entry exists, keeps its `paths:` key, and is scoped to exactly this one
#   Job. Also reciprocal: any OTHER non-vendored KSV-0020 path must be listed here, so this file and
#   test-trivyignore-vendored-operator-boundary.sh cannot drift apart.
#
#   PREMISE (always) — per container: the pod default is high, and the only containers below 10000
#   are the two openbao ones. This is the layer the ignorefile cannot express.
#
#   BEHAVIOUR (when trivy is installed) — the same Job bytes at the dispositioned path and at a
#   probe path, scanned with and without the ignorefile. The ablation must fire, or the suppression
#   below proves nothing.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly IGNOREFILE="$REPO_ROOT/.trivyignore.yaml"
readonly CHECK_ID='KSV-0020'
readonly JOB_PATH='k8s/bases/infrastructure/vault-config/job.yaml'
readonly PROBE_PATH='k8s/bases/apps/trivyignore-identity-probe/job.yaml'
readonly OPENBAO_IMAGE_RE='^quay\.io/openbao/openbao:'
readonly HIGH_UID_FLOOR=10000
readonly EXPECTED_LOW_UID=100
readonly EXPECTED_LOW_CONTAINERS=2

# Vendored bundles carry their own reviewed KSV-0020 disposition, guarded elsewhere.
readonly VENDORED_CDI='k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml'
readonly VENDORED_KUBEVIRT='k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml'

status=0
fail() {
  printf '%s\n' "$*" >&2
  status=1
}

command -v yq >/dev/null 2>&1 || {
  printf 'yq is required to check the vault-config identity disposition boundary\n' >&2
  exit 1
}
[ -r "$IGNOREFILE" ] || {
  printf 'ignorefile not readable: %s\n' "$IGNOREFILE" >&2
  exit 1
}
[ -r "$REPO_ROOT/$JOB_PATH" ] || {
  printf 'Job not readable: %s\n' "$JOB_PATH" >&2
  exit 1
}

# ---------------------------------------------------------------- structure --
# A yq failure must take the missing-entry path rather than become an integer-expression error that
# evaluates false and silently treats the disposition as present.
entries="$(yq "[.misconfigurations[] | select(.id == \"$CHECK_ID\") | select((.paths // []) | contains([\"$JOB_PATH\"]))] | length" "$IGNOREFILE" 2>/dev/null || printf '0')"
entries="${entries:-0}"
[ "$entries" -ge 1 ] || fail \
  "MISSING DISPOSITION: $CHECK_ID has no entry scoped to $JOB_PATH in .trivyignore.yaml"

# Each enumeration below is CAPTURED and its status checked BEFORE its loop runs. A process
# substitution whose command fails feeds the loop nothing, so the body never executes and the check
# reports success having inspected NOTHING — the exact vacuous pass this guard exists to prevent,
# occurring inside the guard itself. A query that did not run is not evidence that a boundary holds.
run_yq() { # 1=expression 2=file 3=what it enumerates, for the failure message
  local out
  if ! out="$(yq -N "$1" "$2" 2>/dev/null)"; then
    fail "QUERY FAILED: could not enumerate $3 — refusing to report a boundary this check never inspected"
    return 1
  fi
  printf '%s\n' "$out"
}

# Reciprocal: every non-vendored path this check is scoped to must be the one reviewed here.
if paths_out="$(run_yq ".misconfigurations[] | select(.id == \"$CHECK_ID\") | (.paths // [])[]" "$IGNOREFILE" "the paths $CHECK_ID is scoped to")"; then
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    case "$path" in
      "$VENDORED_CDI" | "$VENDORED_KUBEVIRT" | "$JOB_PATH") ;;
      *) fail "UNREVIEWED DISPOSITION: $CHECK_ID is scoped to $path, which no premises test guards" ;;
    esac
  done <<PATHS
$paths_out
PATHS
fi

# An entry that lost its paths key suppresses the check repository-wide.
if lens_out="$(run_yq ".misconfigurations[] | select(.id == \"$CHECK_ID\") | (.paths // []) | length" "$IGNOREFILE" "the path counts for $CHECK_ID")"; then
  while IFS= read -r n; do
    [ -n "$n" ] || continue
    if [ "$n" -eq 0 ]; then
      fail "UNSCOPED SKIP: an entry for $CHECK_ID has no paths, which suppresses it on every workload"
    fi
  done <<LENS
$lens_out
LENS
fi

# ------------------------------------------------------------------ premise --
if ! pod_uid="$(run_yq '.spec.template.spec.securityContext.runAsUser // "unset"' "$REPO_ROOT/$JOB_PATH" "the pod-level runAsUser")"; then
  pod_uid="unset"
fi
if [ "$pod_uid" = "unset" ]; then
  fail "PREMISE BROKEN: the pod sets no runAsUser, so every container without an override inherits an unconstrained UID while $CHECK_ID stays suppressed on this Job"
elif [ "$pod_uid" -lt "$HIGH_UID_FLOOR" ]; then
  fail "PREMISE BROKEN: the pod default runAsUser is $pod_uid, below $HIGH_UID_FLOOR. The disposition states that only the two openbao containers run low and that everything else takes a HIGH default."
fi

low=0
if containers_out="$(run_yq '[.spec.template.spec.initContainers[]?, .spec.template.spec.containers[]?] | .[] | .name + "|" + .image + "|" + ((.securityContext.runAsUser // "unset")|tostring)' "$REPO_ROOT/$JOB_PATH" "the containers of the vault-config Job")"; then
  while IFS='|' read -r name image uid; do
    [ -n "$name" ] || continue
    [ "$uid" = "unset" ] && continue
    [ "$uid" -ge "$HIGH_UID_FLOOR" ] && continue
    low=$((low + 1))
    if ! printf '%s' "$image" | grep -qE "$OPENBAO_IMAGE_RE"; then
      fail "PREMISE BROKEN: container '$name' runs as UID $uid from image '$image', which is not an openbao image. The $CHECK_ID disposition covers this whole Job by path, so this container's low UID is now silently suppressed with no image-defined identity to justify it."
    elif [ "$uid" -ne "$EXPECTED_LOW_UID" ]; then
      fail "PREMISE BROKEN: openbao container '$name' runs as UID $uid, not the image-defined $EXPECTED_LOW_UID the disposition names."
    fi
  done <<CONTAINERS
$containers_out
CONTAINERS
  [ "$low" -eq "$EXPECTED_LOW_CONTAINERS" ] || fail \
    "PREMISE BROKEN: $low container(s) run below $HIGH_UID_FLOOR; the disposition is written for exactly $EXPECTED_LOW_CONTAINERS (the two openbao containers)."
fi

[ "$status" -eq 0 ] || exit "$status"
printf 'PASS(structure+premise): %s is scoped to %s; pod default UID %s, exactly %s openbao container(s) below %s.\n' \
  "$CHECK_ID" "$JOB_PATH" "$pod_uid" "$low" "$HIGH_UID_FLOOR"

# ---------------------------------------------------------------- behaviour --
command -v trivy >/dev/null 2>&1 || {
  echo "SKIP(behavioural): trivy not installed; structural and premise checks above still passed"
  exit 0
}
command -v jq >/dev/null 2>&1 || {
  echo "SKIP(behavioural): jq not installed; structural and premise checks above still passed"
  exit 0
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/$(dirname "$JOB_PATH")" "$WORK/$(dirname "$PROBE_PATH")"
cp "$REPO_ROOT/$JOB_PATH" "$WORK/$JOB_PATH"
cp "$REPO_ROOT/$JOB_PATH" "$WORK/$PROBE_PATH"
cp "$IGNOREFILE" "$WORK/.trivyignore.yaml"
[ -d "$REPO_ROOT/.trivy/data" ] && {
  mkdir -p "$WORK/.trivy"
  cp -R "$REPO_ROOT/.trivy/data" "$WORK/.trivy/data"
}

# Byte-identical is the whole point of a paired control — assert it rather than trusting cp.
cmp -s "$WORK/$JOB_PATH" "$WORK/$PROBE_PATH" || {
  printf 'paired control is not paired: the two copies differ, so a path conclusion would be unfounded\n' >&2
  exit 1
}

count_at() {
  jq -r --arg t "$2" --arg id "$CHECK_ID" \
    '[ .Results[]? | select(.Target == $t) | .Misconfigurations[]? | select(.ID == $id and .Status == "FAIL") ] | length' "$1"
}

scan() {
  local out="$1"
  shift
  (cd "$WORK" && trivy fs --scanners misconfig --config-data .trivy/data --format json "$@" .) >"$out" 2>/dev/null
}

# --- Ablation first: without the ignorefile BOTH copies must report, or this proves nothing. ---
scan "$WORK/no-ignore.json"
base_job="$(count_at "$WORK/no-ignore.json" "$JOB_PATH")"
base_probe="$(count_at "$WORK/no-ignore.json" "$PROBE_PATH")"
[ "$base_job" -gt 0 ] || {
  printf 'VACUOUS: without the ignorefile, %s does not fire at %s, so suppressing it below would prove nothing (a trivy rule change?).\n' "$CHECK_ID" "$JOB_PATH" >&2
  exit 1
}
[ "$base_probe" -gt 0 ] || {
  printf 'VACUOUS: without the ignorefile, %s does not fire at %s.\n' "$CHECK_ID" "$PROBE_PATH" >&2
  exit 1
}

# --- With the ignorefile: the dispositioned path is suppressed, the probe is NOT. ---
scan "$WORK/with-ignore.json" --ignorefile .trivyignore.yaml
scoped_job="$(count_at "$WORK/with-ignore.json" "$JOB_PATH")"
scoped_probe="$(count_at "$WORK/with-ignore.json" "$PROBE_PATH")"

[ "$scoped_job" -eq 0 ] || {
  printf 'the disposition does not cover %s (%s finding(s) still reported)\n' "$JOB_PATH" "$scoped_job" >&2
  exit 1
}
[ "$scoped_probe" -gt 0 ] || {
  printf 'BOUNDARY BREACHED: identical bytes at the non-dispositioned path %s are ALSO suppressed, so the %s entry has stopped being path-scoped and a genuinely unjustified low-UID workload would now be hidden. Re-scope the entry in .trivyignore.yaml.\n' "$PROBE_PATH" "$CHECK_ID" >&2
  exit 1
}

printf 'PASS(behaviour): %s is path-scoped.\n' "$CHECK_ID"
printf '  without ignorefile : job=%s  probe=%s   (ablation fired)\n' "$base_job" "$base_probe"
printf '  with    ignorefile : job=%s  probe=%s   (same bytes, path decides)\n' "$scoped_job" "$scoped_probe"
