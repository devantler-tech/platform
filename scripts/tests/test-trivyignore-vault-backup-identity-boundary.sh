#!/usr/bin/env bash
# The KSV-0020/KSV-0021 dispositions on the vault-backup Job and CronJob are PATH-scoped, and this
# test is what keeps them honest.
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
# KSV-0021 rests on a separate, narrower premise: the pod default primary GID is high, while only
# the openbao snapshot container selects the image-defined GID 1000. The mirror still receives
# supplementary group 1000 from fsGroup, so it can read the snapshot's 0640 group entry without a
# low primary GID. This is process membership and does not depend on a PVC setgid bit.
#
# Four layers, which fail for different reasons:
#
#   STRUCTURE (always) — the entry exists, keeps its `paths:` key, and is scoped to exactly these
#   two workloads. Also reciprocal: any OTHER KSV-0020 path must be one of the reviewed sets, so
#   this file and its sibling boundary tests cannot drift apart.
#
#   PREMISE (always) — per container: each pod default UID/GID is high, and the only container below
#   10001 is the openbao `snapshot` one. This is the layer the ignorefile cannot express.
#
#   BEHAVIOUR (when trivy is installed) — the same workload bytes at the dispositioned path and at a
#   probe path, scanned with and without the ignorefile. The ablation must fire, or the suppression
#   below proves nothing.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly IGNOREFILE="$REPO_ROOT/.trivyignore.yaml"
readonly MEGALINTER_CONFIG="$REPO_ROOT/.mega-linter.yml"
readonly UID_CHECK_ID='KSV-0020'
readonly GID_CHECK_ID='KSV-0021'
readonly JOB_PATH='k8s/bases/infrastructure/vault-backup/job.yaml'
readonly CRON_PATH='k8s/bases/infrastructure/vault-backup/cron-job.yaml'
readonly WORKLOAD_PATHS=("$JOB_PATH" "$CRON_PATH")
readonly PROBE_PATH='k8s/bases/apps/trivyignore-vault-backup-probe/job.yaml'
# The disposition rests on account data measured from THIS digest -- /etc/passwd's
# openbao:x:100:1000 and /etc/group's openbao:x:1000 -- not on the repository name. Matching the
# name alone would let an image bump keep the suppression alive while the identity behind it
# changed, which is exactly the premise going stale unnoticed that this guard exists to catch.
# A bump must therefore FAIL here until someone re-measures the new digest and updates this
# constant together with the .trivyignore.yaml statement that quotes it.
readonly EXPECTED_OPENBAO_IMAGE='quay.io/openbao/openbao:2.5.3@sha256:fdc6da21ca6963560c32336fd7feb9cf2d5e52668f1a1647205a4b41171f0806'
# KSV-0020 fires for UID <= 10000, so 10000 is itself LOW and the first safe value is 10001.
# Comparing against 10000 with -lt / -ge would treat exactly 10000 as high: trivy would report it,
# the path-scoped ignore would suppress it, and this guard would still pass -- an off-by-one at
# precisely the boundary it exists to police.
readonly LOW_ID_FLOOR=10001
readonly EXPECTED_LOW_UID=100
readonly EXPECTED_LOW_GID=1000
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

for path in "${WORKLOAD_PATHS[@]}"; do
  entries="$(yq "[.misconfigurations[] | select(.id == \"$GID_CHECK_ID\") | select((.paths // []) | contains([\"$path\"]))] | length" "$IGNOREFILE" 2>/dev/null || printf '0')"
  entries="${entries:-0}"
  [ "$entries" -ge 1 ] || fail \
    "MISSING DISPOSITION: $GID_CHECK_ID has no entry scoped to $path in .trivyignore.yaml"
done

# Each enumeration below is CAPTURED and its status checked BEFORE its loop runs. A process
# substitution whose command fails feeds the loop nothing, so the body never executes and the check
# reports success having inspected NOTHING — the exact vacuous pass this guard exists to prevent,
# occurring inside the guard itself. A query that did not run is not evidence that a boundary holds.
#
# ⚠️ run_yq is ALWAYS called inside $(...), which is a SUBSHELL. A `fail` here would set `status` in
# that subshell and the parent would never see it, so every caller's `if` would simply skip its body
# and the guard would print PASS having inspected nothing — the vacuous pass this whole block exists
# to prevent, occurring inside the mechanism meant to prevent it. It therefore reports the failure to
# stderr and returns non-zero, and the CALLER records it in the parent via query_failed.
query_failed() { # 1=what it enumerates, for the failure message
  fail "QUERY FAILED: could not enumerate $1 — refusing to report a boundary this check never inspected"
}

run_yq() { # 1=expression 2=file 3=what it enumerates, for the failure message
  local out
  if ! out="$(yq -N "$1" "$2" 2>/dev/null)"; then
    printf 'QUERY FAILED: could not enumerate %s\n' "$3" >&2
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
else
  query_failed "the paths $UID_CHECK_ID is scoped to"
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
else
  query_failed "the path counts for $UID_CHECK_ID"
fi

# Reciprocal GID boundary: every path this check is scoped to must be guarded here or by a sibling.
if paths_out="$(run_yq ".misconfigurations[] | select(.id == \"$GID_CHECK_ID\") | (.paths // [])[]" "$IGNOREFILE" "the paths $GID_CHECK_ID is scoped to")"; then
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    reviewed=0
    for known in "${WORKLOAD_PATHS[@]}" "${REVIEWED_ELSEWHERE[@]}"; do
      [ "$path" = "$known" ] && reviewed=1 && break
    done
    [ "$reviewed" -eq 1 ] || fail \
      "UNREVIEWED DISPOSITION: $GID_CHECK_ID is scoped to $path, which no premises test guards"
  done <<PATHS
$paths_out
PATHS
else
  query_failed "the paths $GID_CHECK_ID is scoped to"
fi

if lens_out="$(run_yq ".misconfigurations[] | select(.id == \"$GID_CHECK_ID\") | (.paths // []) | length" "$IGNOREFILE" "the path counts for $GID_CHECK_ID")"; then
  while IFS= read -r n; do
    [ -n "$n" ] || continue
    [ "$n" -gt 0 ] || fail \
      "UNSCOPED SKIP: an entry for $GID_CHECK_ID has no paths, which suppresses it on every workload"
  done <<LENS
$lens_out
LENS
else
  query_failed "the path counts for $GID_CHECK_ID"
fi

disabled="$(yq '[.DISABLE_ERRORS_LINTERS[]? | select(. == "REPOSITORY_TRIVY")] | length' "$MEGALINTER_CONFIG" 2>/dev/null || printf '1')"
[ "${disabled:-1}" -eq 0 ] || fail \
  'GATE STILL SOFT: REPOSITORY_TRIVY remains in DISABLE_ERRORS_LINTERS'

# ------------------------------------------------------------------ premise --
for path in "${WORKLOAD_PATHS[@]}"; do
  if ! pod_uid="$(run_yq "$POD_SPEC.securityContext.runAsUser // \"unset\"" "$REPO_ROOT/$path" "the pod-level runAsUser of $path")"; then
    query_failed "the pod-level runAsUser of $path"
    pod_uid="unset"
  fi
  if [ "$pod_uid" = "unset" ]; then
    fail "PREMISE BROKEN: $path sets no pod-level runAsUser, so every container without an override inherits an unconstrained UID while $UID_CHECK_ID stays suppressed on it"
  elif [ "$pod_uid" -lt "$LOW_ID_FLOOR" ]; then
    fail "PREMISE BROKEN: the pod default runAsUser in $path is $pod_uid, which KSV-0020 counts as low (<= 10000). The disposition states that only the openbao snapshot container runs low and that everything else takes a HIGH default."
  fi

  if ! pod_gid="$(run_yq "$POD_SPEC.securityContext.runAsGroup // \"unset\"" "$REPO_ROOT/$path" "the pod-level runAsGroup of $path")"; then
    query_failed "the pod-level runAsGroup of $path"
    pod_gid="unset"
  fi
  if [ "$pod_gid" = "unset" ]; then
    fail "PREMISE BROKEN: $path sets no pod-level runAsGroup, so containers without overrides inherit an unconstrained primary GID"
  elif [ "$pod_gid" -lt "$LOW_ID_FLOOR" ]; then
    fail "PREMISE BROKEN: the pod default runAsGroup in $path is $pod_gid, which KSV-0021 counts as low (<= 10000). Only the openbao snapshot container may use the image-defined low GID."
  fi

  if ! fs_group="$(run_yq "$POD_SPEC.securityContext.fsGroup // \"unset\"" "$REPO_ROOT/$path" "the fsGroup of $path")"; then
    query_failed "the fsGroup of $path"
    fs_group="unset"
  fi
  [ "$fs_group" = "$EXPECTED_LOW_GID" ] || fail \
    "PREMISE BROKEN: fsGroup in $path is $fs_group, not $EXPECTED_LOW_GID; the high-GID mirror would lose supplementary access to 0640 snapshots"

  low=0
  if containers_out="$(run_yq "[$POD_SPEC.initContainers[]?, $POD_SPEC.containers[]?] | .[] | .name + \"|\" + .image + \"|\" + ((.securityContext.runAsUser // \"unset\")|tostring)" "$REPO_ROOT/$path" "the containers of $path")"; then
    while IFS='|' read -r name image uid; do
      [ -n "$name" ] || continue
      [ "$uid" = "unset" ] && continue
      [ "$uid" -ge "$LOW_ID_FLOOR" ] && continue
      low=$((low + 1))
      if [ "$image" != "$EXPECTED_OPENBAO_IMAGE" ]; then
        fail "PREMISE BROKEN: container '$name' in $path runs as UID $uid from image '$image', which is not the pinned openbao image whose measured uid-100 account justifies $UID_CHECK_ID. If this is a deliberate bump, re-measure /etc/passwd and /etc/group at the new digest and update EXPECTED_OPENBAO_IMAGE and the .trivyignore.yaml statement together."
      elif [ "$uid" -ne "$EXPECTED_LOW_UID" ]; then
        fail "PREMISE BROKEN: openbao container '$name' in $path runs as UID $uid, not the image-defined $EXPECTED_LOW_UID the disposition names."
      elif [ "$name" != "$EXPECTED_LOW_CONTAINER_NAME" ]; then
        fail "PREMISE BROKEN: the low-UID container in $path is '$name', not the '$EXPECTED_LOW_CONTAINER_NAME' container the disposition names."
      fi
    done <<CONTAINERS
$containers_out
CONTAINERS
    [ "$low" -eq "$EXPECTED_LOW_CONTAINERS" ] || fail \
      "PREMISE BROKEN: $low container(s) in $path run at a UID KSV-0020 counts as low (<= 10000); the disposition is written for exactly $EXPECTED_LOW_CONTAINERS (the openbao snapshot container)."
  else
    query_failed "the containers of $path"
  fi
  low_gid=0
  if containers_out="$(run_yq "[$POD_SPEC.initContainers[]?, $POD_SPEC.containers[]?] | .[] | .name + \"|\" + .image + \"|\" + ((.securityContext.runAsGroup // ($POD_SPEC.securityContext.runAsGroup // \"unset\"))|tostring)" "$REPO_ROOT/$path" "the effective container GIDs of $path")"; then
    while IFS='|' read -r name image gid; do
      [ -n "$name" ] || continue
      [ "$gid" = "unset" ] && continue
      [ "$gid" -ge "$LOW_ID_FLOOR" ] && continue
      low_gid=$((low_gid + 1))
      [ "$name" = "$EXPECTED_LOW_CONTAINER_NAME" ] || fail \
        "PREMISE BROKEN: container '$name' in $path has low effective GID $gid; only '$EXPECTED_LOW_CONTAINER_NAME' is justified"
      [ "$image" = "$EXPECTED_OPENBAO_IMAGE" ] || fail \
        "PREMISE BROKEN: low-GID container '$name' in $path does not use the measured openbao image"
      [ "$gid" -eq "$EXPECTED_LOW_GID" ] || fail \
        "PREMISE BROKEN: openbao container '$name' in $path uses GID $gid, not image-defined $EXPECTED_LOW_GID"
    done <<CONTAINERS
$containers_out
CONTAINERS
    [ "$low_gid" -eq "$EXPECTED_LOW_CONTAINERS" ] || fail \
      "PREMISE BROKEN: $low_gid container(s) in $path have a low effective primary GID; expected only the openbao snapshot container"
  else
    query_failed "the effective container GIDs of $path"
  fi
done

[ "$status" -eq 0 ] || exit "$status"
printf 'PASS(structure+premise+gate): %s and %s are path-scoped to the two vault-backup workloads and Trivy is blocking.\n' \
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
  gid_base="$(count_at "$WORK/no-ignore.json" "$path" "$GID_CHECK_ID")"
  [ "$gid_base" -gt 0 ] || {
    printf 'VACUOUS: without the ignorefile, %s does not fire at %s, so suppressing it below would prove nothing.\n' "$GID_CHECK_ID" "$path" >&2
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
  gid_scoped="$(count_at "$WORK/with-ignore.json" "$path" "$GID_CHECK_ID")"
  [ "$gid_scoped" -eq 0 ] || {
    printf 'the %s disposition does not cover %s (%s finding(s) still reported)\n' "$GID_CHECK_ID" "$path" "$gid_scoped" >&2
    exit 1
  }
done

scoped_probe="$(count_at "$WORK/with-ignore.json" "$PROBE_PATH" "$UID_CHECK_ID")"
[ "$scoped_probe" -gt 0 ] || {
  printf 'BOUNDARY BREACHED: identical bytes at the non-dispositioned path %s are ALSO suppressed, so the %s entry has stopped being path-scoped and a genuinely unjustified low-identity workload would now be hidden. Re-scope the entry in .trivyignore.yaml.\n' "$PROBE_PATH" "$UID_CHECK_ID" >&2
  exit 1
}

gid_probe="$(count_at "$WORK/with-ignore.json" "$PROBE_PATH" "$GID_CHECK_ID")"
[ "$gid_probe" -gt 0 ] || {
  printf 'BOUNDARY BREACHED: identical bytes at the non-dispositioned path %s are also suppressed for %s.\n' "$PROBE_PATH" "$GID_CHECK_ID" >&2
  exit 1
}

printf 'PASS(behaviour): %s and %s are path-scoped; identical probe bytes remain reported.\n' "$UID_CHECK_ID" "$GID_CHECK_ID"
