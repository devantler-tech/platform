#!/usr/bin/env bash
# The KSV-0020 disposition on the vault-backup Job and CronJob is PATH-scoped, and this test is what
# keeps it honest.
#
# WHY THIS EXISTS
# Each workload runs two containers. `snapshot` sets `runAsUser: 100`, which is the openbao image's
# own baked identity (`openbao:x:100:1000` in its /etc/passwd, verified against the pinned digest —
# the SAME digest the vault-config Job pins, so that measurement is a measurement of this image
# rather than an analogy to it). The `minio/mc` mirror container bakes no such identity, so each
# workload sets runAsUser PER CONTAINER and leaves the mirror on the pod's high 65532 default. The
# same reasoning is already written into each workload's resource-scoped `checkov.io/skip2`
# annotation.
#
# THE SCOPING GRANULARITIES DO NOT MATCH, AND THAT IS THE WHOLE RISK.
# `checkov.io/skipN` is scoped to a RESOURCE; a trivy ignorefile entry is scoped to a PATH. So the
# trivy entry silences KSV-0020 for every container in these workloads, including one added later at
# a low UID for no reason at all. Nothing in .trivyignore.yaml can express "only the snapshot
# container", so it is asserted here.
#
# Nothing else would fail if that happened: the scan still runs, the count still drops, and the gate
# still reports a smaller number, which reads exactly like progress.
#
# ⚠️ These workloads ship to PRODUCTION (k8s/providers/hetzner/infrastructure/ includes
# k8s/bases/infrastructure/), so the "local/CI provider only" premise that backs the vendored
# operator dispositions is NOT available here and is not borrowed. The disposition stands on the
# image-defined identity alone — which is exactly why that identity is asserted rather than trusted.
#
# ⚠️ UID ONLY, DELIBERATELY. KSV-0021 is NOT dispositioned on these workloads: the pod default
# runAsGroup is 1000, so the mirror INHERITS a low GID chosen for openbao instead of taking a high
# default. vault-config can make that split because its shared paths are emptyDir, where fsGroup's
# setgid bit carries the group whatever gid the writer runs as (#3258); these snapshots live on a
# PVC, where that is not established — #3281 assumed it transferred and was withdrawn. This test
# asserts that absence, so a later run cannot quietly borrow the UID premise for the GID half.
#
# Four layers, which fail for different reasons:
#
#   STRUCTURE (always) — the entry exists, keeps its `paths:` key, and is scoped to exactly these
#   two workloads. Also reciprocal: any OTHER KSV-0020 path must be one of the reviewed sets, so
#   this file and its sibling boundary tests cannot drift apart.
#
#   ABSENCE (always) — KSV-0021 is NOT scoped to either workload.
#
#   PREMISE (always) — per container: each pod default UID is high, and the only container below
#   10000 is the openbao `snapshot` one. This is the layer the ignorefile cannot express.
#
#   BEHAVIOUR (when trivy is installed) — the same workload bytes at the dispositioned path and at a
#   probe path, scanned with and without the ignorefile. The ablation must fire, or the suppression
#   below proves nothing.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly IGNOREFILE="$REPO_ROOT/.trivyignore.yaml"
readonly UID_CHECK_ID='KSV-0020'
readonly GID_CHECK_ID='KSV-0021'
readonly JOB_PATH='k8s/bases/infrastructure/vault-backup/job.yaml'
readonly CRON_PATH='k8s/bases/infrastructure/vault-backup/cron-job.yaml'
readonly WORKLOAD_PATHS=("$JOB_PATH" "$CRON_PATH")
readonly PROBE_PATH='k8s/bases/apps/trivyignore-vault-backup-probe/job.yaml'
readonly OPENBAO_IMAGE_RE='^quay\.io/openbao/openbao:'
readonly LOW_ID_FLOOR=10000
readonly EXPECTED_LOW_UID=100
readonly EXPECTED_LOW_CONTAINERS=1
readonly EXPECTED_LOW_CONTAINER_NAME='snapshot'

# Paths carrying their own reviewed KSV-0020 disposition, guarded by sibling tests.
readonly REVIEWED_ELSEWHERE=(
  'k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml'
  'k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml'
  'k8s/bases/infrastructure/vault-config/job.yaml'
)

# A Job keeps its pod spec one level down; a CronJob keeps it under jobTemplate. Selecting whichever
# exists lets one expression serve both without the caller stating which kind it is.
readonly POD_SPEC='(.spec.template.spec // .spec.jobTemplate.spec.template.spec)'

status=0
fail() {
  printf '%s\n' "$*" >&2
  status=1
}

command -v yq >/dev/null 2>&1 || {
  printf 'yq is required to check the vault-backup identity disposition boundary\n' >&2
  exit 1
}
[ -r "$IGNOREFILE" ] || {
  printf 'ignorefile not readable: %s\n' "$IGNOREFILE" >&2
  exit 1
}
for path in "${WORKLOAD_PATHS[@]}"; do
  [ -r "$REPO_ROOT/$path" ] || {
    printf 'workload not readable: %s\n' "$path" >&2
    exit 1
  }
done

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

# ---------------------------------------------------------------- structure --
for path in "${WORKLOAD_PATHS[@]}"; do
  # A yq failure must take the missing-entry path rather than become an integer-expression error
  # that evaluates false and silently treats the disposition as present.
  entries="$(yq "[.misconfigurations[] | select(.id == \"$UID_CHECK_ID\") | select((.paths // []) | contains([\"$path\"]))] | length" "$IGNOREFILE" 2>/dev/null || printf '0')"
  entries="${entries:-0}"
  [ "$entries" -ge 1 ] || fail \
    "MISSING DISPOSITION: $UID_CHECK_ID has no entry scoped to $path in .trivyignore.yaml"
done

# Reciprocal: every path this check is scoped to must be one a premises test reviews.
if paths_out="$(run_yq ".misconfigurations[] | select(.id == \"$UID_CHECK_ID\") | (.paths // [])[]" "$IGNOREFILE" "the paths $UID_CHECK_ID is scoped to")"; then
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    reviewed=0
    for known in "${WORKLOAD_PATHS[@]}" "${REVIEWED_ELSEWHERE[@]}"; do
      [ "$path" = "$known" ] && reviewed=1 && break
    done
    [ "$reviewed" -eq 1 ] || fail \
      "UNREVIEWED DISPOSITION: $UID_CHECK_ID is scoped to $path, which no premises test guards"
  done <<PATHS
$paths_out
PATHS
fi

# An entry that lost its paths key suppresses the check repository-wide.
if lens_out="$(run_yq ".misconfigurations[] | select(.id == \"$UID_CHECK_ID\") | (.paths // []) | length" "$IGNOREFILE" "the path counts for $UID_CHECK_ID")"; then
  while IFS= read -r n; do
    [ -n "$n" ] || continue
    if [ "$n" -eq 0 ]; then
      fail "UNSCOPED SKIP: an entry for $UID_CHECK_ID has no paths, which suppresses it on every workload"
    fi
  done <<LENS
$lens_out
LENS
fi

# ------------------------------------------------------------------ absence --
# The GID half is deliberately NOT dispositioned here. Asserting that keeps a later run from
# borrowing the UID premise for a claim the PVC sharing model has not established (#3202, #3281).
for path in "${WORKLOAD_PATHS[@]}"; do
  gid_entries="$(yq "[.misconfigurations[] | select(.id == \"$GID_CHECK_ID\") | select((.paths // []) | contains([\"$path\"]))] | length" "$IGNOREFILE" 2>/dev/null || printf '1')"
  gid_entries="${gid_entries:-1}"
  [ "$gid_entries" -eq 0 ] || fail \
    "UNJUSTIFIED WIDENING: $GID_CHECK_ID is now scoped to $path. The mirror container inherits GID 1000 from the pod and minio/mc bakes no such identity, so the image-defined premise that carries $UID_CHECK_ID does NOT carry this. Establish how the mirror reads the snapshots on the PVC first (#3202)."
done

# ------------------------------------------------------------------ premise --
for path in "${WORKLOAD_PATHS[@]}"; do
  if ! pod_uid="$(run_yq "$POD_SPEC.securityContext.runAsUser // \"unset\"" "$REPO_ROOT/$path" "the pod-level runAsUser of $path")"; then
    pod_uid="unset"
  fi
  if [ "$pod_uid" = "unset" ]; then
    fail "PREMISE BROKEN: $path sets no pod-level runAsUser, so every container without an override inherits an unconstrained UID while $UID_CHECK_ID stays suppressed on it"
  elif [ "$pod_uid" -lt "$LOW_ID_FLOOR" ]; then
    fail "PREMISE BROKEN: the pod default runAsUser in $path is $pod_uid, below $LOW_ID_FLOOR. The disposition states that only the openbao snapshot container runs low and that everything else takes a HIGH default."
  fi

  low=0
  if containers_out="$(run_yq "[$POD_SPEC.initContainers[]?, $POD_SPEC.containers[]?] | .[] | .name + \"|\" + .image + \"|\" + ((.securityContext.runAsUser // \"unset\")|tostring)" "$REPO_ROOT/$path" "the containers of $path")"; then
    while IFS='|' read -r name image uid; do
      [ -n "$name" ] || continue
      [ "$uid" = "unset" ] && continue
      [ "$uid" -ge "$LOW_ID_FLOOR" ] && continue
      low=$((low + 1))
      if ! printf '%s' "$image" | grep -qE "$OPENBAO_IMAGE_RE"; then
        fail "PREMISE BROKEN: container '$name' in $path runs as UID $uid from image '$image', which is not an openbao image. The $UID_CHECK_ID disposition covers this whole workload by path, so this container's low UID is now silently suppressed with no image-defined identity to justify it."
      elif [ "$uid" -ne "$EXPECTED_LOW_UID" ]; then
        fail "PREMISE BROKEN: openbao container '$name' in $path runs as UID $uid, not the image-defined $EXPECTED_LOW_UID the disposition names."
      elif [ "$name" != "$EXPECTED_LOW_CONTAINER_NAME" ]; then
        fail "PREMISE BROKEN: the low-UID container in $path is '$name', not the '$EXPECTED_LOW_CONTAINER_NAME' container the disposition names."
      fi
    done <<CONTAINERS
$containers_out
CONTAINERS
    [ "$low" -eq "$EXPECTED_LOW_CONTAINERS" ] || fail \
      "PREMISE BROKEN: $low container(s) in $path run below $LOW_ID_FLOOR by UID; the disposition is written for exactly $EXPECTED_LOW_CONTAINERS (the openbao snapshot container)."
  fi
done

[ "$status" -eq 0 ] || exit "$status"
printf 'PASS(structure+absence+premise): %s is scoped to the two vault-backup workloads; %s is not.\n' \
  "$UID_CHECK_ID" "$GID_CHECK_ID"

# ---------------------------------------------------------------- behaviour --
command -v trivy >/dev/null 2>&1 || {
  echo "SKIP(behavioural): trivy not installed; structural, absence and premise checks above still passed"
  exit 0
}
command -v jq >/dev/null 2>&1 || {
  echo "SKIP(behavioural): jq not installed; structural, absence and premise checks above still passed"
  exit 0
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/$(dirname "$JOB_PATH")" "$WORK/$(dirname "$PROBE_PATH")"
cp "$REPO_ROOT/$JOB_PATH" "$WORK/$JOB_PATH"
cp "$REPO_ROOT/$CRON_PATH" "$WORK/$CRON_PATH"
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

count_at() { # 1=scan json  2=target path  3=check id
  jq -r --arg t "$2" --arg id "$3" \
    '[ .Results[]? | select(.Target == $t) | .Misconfigurations[]? | select(.ID == $id and .Status == "FAIL") ] | length' "$1"
}

scan() {
  local out="$1"
  shift
  (cd "$WORK" && trivy fs --scanners misconfig --config-data .trivy/data --format json "$@" .) >"$out" 2>/dev/null
}

scan "$WORK/no-ignore.json"
scan "$WORK/with-ignore.json" --ignorefile .trivyignore.yaml

# --- Ablation first: without the ignorefile every copy must report, or this proves nothing. ---
for path in "${WORKLOAD_PATHS[@]}" "$PROBE_PATH"; do
  base="$(count_at "$WORK/no-ignore.json" "$path" "$UID_CHECK_ID")"
  [ "$base" -gt 0 ] || {
    printf 'VACUOUS: without the ignorefile, %s does not fire at %s, so suppressing it below would prove nothing (a trivy rule change?).\n' "$UID_CHECK_ID" "$path" >&2
    exit 1
  }
done

# --- With the ignorefile: the dispositioned paths are suppressed, the probe is NOT. ---
for path in "${WORKLOAD_PATHS[@]}"; do
  scoped="$(count_at "$WORK/with-ignore.json" "$path" "$UID_CHECK_ID")"
  [ "$scoped" -eq 0 ] || {
    printf 'the %s disposition does not cover %s (%s finding(s) still reported)\n' "$UID_CHECK_ID" "$path" "$scoped" >&2
    exit 1
  }
done

scoped_probe="$(count_at "$WORK/with-ignore.json" "$PROBE_PATH" "$UID_CHECK_ID")"
[ "$scoped_probe" -gt 0 ] || {
  printf 'BOUNDARY BREACHED: identical bytes at the non-dispositioned path %s are ALSO suppressed, so the %s entry has stopped being path-scoped and a genuinely unjustified low-identity workload would now be hidden. Re-scope the entry in .trivyignore.yaml.\n' "$PROBE_PATH" "$UID_CHECK_ID" >&2
  exit 1
}

# The GID half must still be REPORTED on these workloads — the absence layer above checks the
# ignorefile, this checks what the scanner actually does with it.
for path in "${WORKLOAD_PATHS[@]}"; do
  gid_scoped="$(count_at "$WORK/with-ignore.json" "$path" "$GID_CHECK_ID")"
  [ "$gid_scoped" -gt 0 ] || {
    printf 'SUPPRESSED WITHOUT A PREMISE: %s no longer reports at %s. It is deliberately not dispositioned there (#3202) — if a fix made it genuinely clean, update this test and the ignorefile comment together.\n' "$GID_CHECK_ID" "$path" >&2
    exit 1
  }
done

printf 'PASS(behaviour): %s is path-scoped and %s remains reported.\n' "$UID_CHECK_ID" "$GID_CHECK_ID"
