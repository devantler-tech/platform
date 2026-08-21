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
readonly UID_CHECK_ID='KSV-0020'
readonly GID_CHECK_ID='KSV-0021'
# Both dispositions on this Job are path-scoped and rest on the SAME per-container premise, so
# they are structurally and behaviourally checked together rather than drifting apart.
readonly CHECK_IDS=("$UID_CHECK_ID" "$GID_CHECK_ID")
readonly JOB_PATH='k8s/bases/infrastructure/vault-config/job.yaml'
readonly PROBE_PATH='k8s/bases/apps/trivyignore-identity-probe/job.yaml'
# Pinned to the exact reviewed reference, not the repository name: the disposition rests on account
# data measured from THIS digest, so an image bump must fail here until the identity is re-measured.
readonly EXPECTED_OPENBAO_IMAGE='quay.io/openbao/openbao:2.5.3@sha256:fdc6da21ca6963560c32336fd7feb9cf2d5e52668f1a1647205a4b41171f0806'
readonly HIGH_UID_FLOOR=10000
readonly EXPECTED_LOW_UID=100
readonly EXPECTED_LOW_GID=1000
readonly EXPECTED_LOW_CONTAINERS=2

# Vendored bundles carry their own reviewed KSV-0020 disposition, guarded elsewhere.
readonly VENDORED_CDI='k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml'
readonly VENDORED_KUBEVIRT='k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml'

# The vault-backup Job and CronJob carry their own reviewed KSV-0020 disposition on the same
# image-defined identity, guarded by test-trivyignore-vault-backup-identity-boundary.sh.
readonly VAULT_BACKUP_JOB='k8s/bases/infrastructure/vault-backup/job.yaml'
readonly VAULT_BACKUP_CRON='k8s/bases/infrastructure/vault-backup/cron-job.yaml'

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

# Both dispositions are scoped to this Job and rest on the same per-container premise, so each is
# asserted identically. Checking only one would let the other lose its scoping unnoticed.
for CHECK_ID in "${CHECK_IDS[@]}"; do
  # A yq failure must take the missing-entry path rather than become an integer-expression error that
  # evaluates false and silently treats the disposition as present.
  entries="$(yq "[.misconfigurations[] | select(.id == \"$CHECK_ID\") | select((.paths // []) | contains([\"$JOB_PATH\"]))] | length" "$IGNOREFILE" 2>/dev/null || printf '0')"
  entries="${entries:-0}"
  [ "$entries" -ge 1 ] || fail \
    "MISSING DISPOSITION: $CHECK_ID has no entry scoped to $JOB_PATH in .trivyignore.yaml"

  # Reciprocal: every non-vendored path this check is scoped to must be the one reviewed here.
  if paths_out="$(run_yq ".misconfigurations[] | select(.id == \"$CHECK_ID\") | (.paths // [])[]" "$IGNOREFILE" "the paths $CHECK_ID is scoped to")"; then
    while IFS= read -r path; do
      [ -n "$path" ] || continue
      case "$path" in
        "$VENDORED_CDI" | "$VENDORED_KUBEVIRT" | "$JOB_PATH" | "$VAULT_BACKUP_JOB" | "$VAULT_BACKUP_CRON") ;;
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
done
# ------------------------------------------------------------------ premise --
if ! pod_uid="$(run_yq '.spec.template.spec.securityContext.runAsUser // "unset"' "$REPO_ROOT/$JOB_PATH" "the pod-level runAsUser")"; then
  pod_uid="unset"
fi
if [ "$pod_uid" = "unset" ]; then
  fail "PREMISE BROKEN: the pod sets no runAsUser, so every container without an override inherits an unconstrained UID while $UID_CHECK_ID stays suppressed on this Job"
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
    if [ "$image" != "$EXPECTED_OPENBAO_IMAGE" ]; then
      fail "PREMISE BROKEN: container '$name' runs as UID $uid from image '$image', which is not the pinned openbao image whose measured account justifies this. The $UID_CHECK_ID disposition covers this whole Job by path, so this container's low UID is now silently suppressed with no image-defined identity to justify it."
    elif [ "$uid" -ne "$EXPECTED_LOW_UID" ]; then
      fail "PREMISE BROKEN: openbao container '$name' runs as UID $uid, not the image-defined $EXPECTED_LOW_UID the disposition names."
    fi
  done <<CONTAINERS
$containers_out
CONTAINERS
  [ "$low" -eq "$EXPECTED_LOW_CONTAINERS" ] || fail \
    "PREMISE BROKEN: $low container(s) run below $HIGH_UID_FLOOR; the disposition is written for exactly $EXPECTED_LOW_CONTAINERS (the two openbao containers)."
fi

# --- GID: the same premise, one field over. A container can carry a justified low UID and an
# --- unjustified low GID independently, so the two are asserted separately.
if ! pod_gid="$(run_yq '.spec.template.spec.securityContext.runAsGroup // "unset"' "$REPO_ROOT/$JOB_PATH" "the pod-level runAsGroup")"; then
  pod_gid="unset"
fi
if [ "$pod_gid" = "unset" ]; then
  fail "PREMISE BROKEN: the pod sets no runAsGroup, so every container without an override inherits an unconstrained GID while $GID_CHECK_ID stays suppressed on this Job"
elif [ "$pod_gid" -lt "$HIGH_UID_FLOOR" ]; then
  fail "PREMISE BROKEN: the pod default runAsGroup is $pod_gid, below $HIGH_UID_FLOOR. The $GID_CHECK_ID disposition states that only the two openbao containers run low and that everything else takes a HIGH default."
fi

# fsGroup is what makes the GID split cost no access: it sets the emptyDir volumes' group AND the
# setgid bit, so files inherit group 1000 whatever gid their writer runs as, and it joins every
# container's supplementary groups. Drop it and the split stops being free — the containers on the
# high default silently lose access to what openbao wrote, and vice versa (#3258).
if ! fs_group="$(run_yq '.spec.template.spec.securityContext.fsGroup // "unset"' "$REPO_ROOT/$JOB_PATH" "the pod-level fsGroup")"; then
  fs_group="unset"
fi
[ "$fs_group" = "$EXPECTED_LOW_GID" ] || fail \
  "PREMISE BROKEN: fsGroup is '$fs_group', not $EXPECTED_LOW_GID. The per-container GID split depends on fsGroup supplying shared-volume access; without it, raising the pod default breaks cross-container reads instead of merely raising a group."

low_gid=0
if gids_out="$(run_yq '[.spec.template.spec.initContainers[]?, .spec.template.spec.containers[]?] | .[] | .name + "|" + .image + "|" + ((.securityContext.runAsGroup // "unset")|tostring)' "$REPO_ROOT/$JOB_PATH" "the container GIDs of the vault-config Job")"; then
  while IFS='|' read -r name image gid; do
    [ -n "$name" ] || continue
    [ "$gid" = "unset" ] && continue
    [ "$gid" -ge "$HIGH_UID_FLOOR" ] && continue
    low_gid=$((low_gid + 1))
    if [ "$image" != "$EXPECTED_OPENBAO_IMAGE" ]; then
      fail "PREMISE BROKEN: container '$name' runs as GID $gid from image '$image', which is not the pinned openbao image whose measured account justifies this. The $GID_CHECK_ID disposition covers this whole Job by path, so this container's low GID is now silently suppressed with no image-defined identity to justify it."
    elif [ "$gid" -ne "$EXPECTED_LOW_GID" ]; then
      fail "PREMISE BROKEN: openbao container '$name' runs as GID $gid, not the image-defined $EXPECTED_LOW_GID the disposition names."
    fi
  done <<GIDS
$gids_out
GIDS
  [ "$low_gid" -eq "$EXPECTED_LOW_CONTAINERS" ] || fail \
    "PREMISE BROKEN: $low_gid container(s) run below $HIGH_UID_FLOOR by GID; the $GID_CHECK_ID disposition is written for exactly $EXPECTED_LOW_CONTAINERS (the two openbao containers)."
fi
[ "$status" -eq 0 ] || exit "$status"
printf 'PASS(structure+premise): %s and %s are scoped to %s.\n' \
  "$UID_CHECK_ID" "$GID_CHECK_ID" "$JOB_PATH"
printf '  pod defaults: uid=%s gid=%s fsGroup=%s (floor %s)\n' \
  "$pod_uid" "$pod_gid" "$fs_group" "$HIGH_UID_FLOOR"
printf '  below the floor: %s container(s) by UID, %s by GID; expected %s each (the openbao pair)\n' \
  "$low" "$low_gid" "$EXPECTED_LOW_CONTAINERS"

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

count_at() { # 1=scan json  2=target path  3=check id
  jq -r --arg t "$2" --arg id "$3" \
    '[ .Results[]? | select(.Target == $t) | .Misconfigurations[]? | select(.ID == $id and .Status == "FAIL") ] | length' "$1"
}

scan() {
  local out="$1"
  shift
  (cd "$WORK" && trivy fs --scanners misconfig --config-data .trivy/data --format json "$@" .) >"$out" 2>/dev/null
}

# The two scans do not depend on the check id, so run them once and assert per check below.
scan "$WORK/no-ignore.json"
scan "$WORK/with-ignore.json" --ignorefile .trivyignore.yaml

for CHECK_ID in "${CHECK_IDS[@]}"; do
  # --- Ablation first: without the ignorefile BOTH copies must report, or this proves nothing. ---
  base_job="$(count_at "$WORK/no-ignore.json" "$JOB_PATH" "$CHECK_ID")"
  base_probe="$(count_at "$WORK/no-ignore.json" "$PROBE_PATH" "$CHECK_ID")"
  [ "$base_job" -gt 0 ] || {
    printf 'VACUOUS: without the ignorefile, %s does not fire at %s, so suppressing it below would prove nothing (a trivy rule change?).\n' "$CHECK_ID" "$JOB_PATH" >&2
    exit 1
  }
  [ "$base_probe" -gt 0 ] || {
    printf 'VACUOUS: without the ignorefile, %s does not fire at %s.\n' "$CHECK_ID" "$PROBE_PATH" >&2
    exit 1
  }

  # --- With the ignorefile: the dispositioned path is suppressed, the probe is NOT. ---
  scoped_job="$(count_at "$WORK/with-ignore.json" "$JOB_PATH" "$CHECK_ID")"
  scoped_probe="$(count_at "$WORK/with-ignore.json" "$PROBE_PATH" "$CHECK_ID")"

  [ "$scoped_job" -eq 0 ] || {
    printf 'the %s disposition does not cover %s (%s finding(s) still reported)\n' "$CHECK_ID" "$JOB_PATH" "$scoped_job" >&2
    exit 1
  }
  [ "$scoped_probe" -gt 0 ] || {
    printf 'BOUNDARY BREACHED: identical bytes at the non-dispositioned path %s are ALSO suppressed, so the %s entry has stopped being path-scoped and a genuinely unjustified low-identity workload would now be hidden. Re-scope the entry in .trivyignore.yaml.\n' "$PROBE_PATH" "$CHECK_ID" >&2
    exit 1
  }

  printf 'PASS(behaviour): %s is path-scoped.\n' "$CHECK_ID"
  printf '  without ignorefile : job=%s  probe=%s   (ablation fired)\n' "$base_job" "$base_probe"
  printf '  with    ignorefile : job=%s  probe=%s   (same bytes, path decides)\n' "$scoped_job" "$scoped_probe"
done
