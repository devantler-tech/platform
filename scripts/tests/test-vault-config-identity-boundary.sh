#!/usr/bin/env bash
# The vault-config Job runs a LOW uid and gid for exactly two of its four containers, and this
# test is what keeps it at exactly two.
#
# WHY THIS EXISTS
# `checkov.io/skipN` is RESOURCE-scoped: the skip2 annotation silences CKV_K8S_40 for every
# container in this Job, not just the ones its rationale accounts for. That rationale is narrow —
# uid 100 / gid 1000 is the openbao image's own baked identity (`openbao:x:100:1000` in its
# /etc/passwd), which `vault-init` and `vault-config` restate. `mc` and `alpine/k8s` bake no such
# identity, so they take the pod's high 65532 defaults.
#
# The risk that annotation creates is NOT that it is wrong; it is that its blast radius silently
# grows. Move a low `runAsUser`/`runAsGroup` back up to pod level, or add a fifth container that
# inherits one, and that execution is hidden by a suppression written for a different container.
# Nothing else would fail: the manifest still applies, the scan still passes, and the count still
# reads zero — which looks exactly like compliance.
#
# The gid half additionally depends on fsGroup rather than on a shared primary gid. Kubernetes
# sets gid 1000 and the setgid bit on the emptyDir volumes, so files created in /shared and
# /snapshots inherit group 1000 whatever gid their writer runs as, and fsGroup is added to every
# container's supplementary groups so the others can still read them (#3258). Drop `fsGroup` and
# the UID split stops being safe, so it is asserted here too.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly manifest="$repo_root/k8s/bases/infrastructure/vault-config/job.yaml"

command -v yq >/dev/null 2>&1 || {
  printf 'yq is required to check the vault-config identity boundary\n' >&2
  exit 1
}
[ -r "$manifest" ] || {
  printf 'FAIL: cannot read %s\n' "$manifest" >&2
  exit 1
}

# The only two containers whose image bakes the low identity.
readonly openbao_containers=(vault-init vault-config)
readonly low_uid=100 low_gid=1000 high_id=65532

status=0
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  status=1
}

# ------------------------------------------------------------------ pod defaults --
pod_uid="$(yq -e '.spec.template.spec.securityContext.runAsUser' "$manifest")"
pod_gid="$(yq -e '.spec.template.spec.securityContext.runAsGroup' "$manifest")"
pod_fsgroup="$(yq -e '.spec.template.spec.securityContext.fsGroup' "$manifest")"

[ "$pod_uid" -gt 10000 ] || fail "pod runAsUser must be > 10000 so no container inherits a low uid (got $pod_uid)"
[ "$pod_gid" -gt 10000 ] || fail "pod runAsGroup must be > 10000 so no container inherits a low gid (got $pod_gid)"
[ "$pod_fsgroup" -eq "$low_gid" ] || fail "fsGroup must be $low_gid — it is what preserves cross-container access once the primary gids differ (got $pod_fsgroup)"

# ------------------------------------------------------------ per-container split --
# Capture first, then iterate: a failing yq must not leave the loop body unrun and the check
# reporting success on an empty list.
names="$(yq -e '[.spec.template.spec.initContainers[], .spec.template.spec.containers[]] | .[].name' "$manifest")" ||
  {
    printf 'FAIL: could not enumerate containers in %s\n' "$manifest" >&2
    exit 1
  }
[ -n "$names" ] || {
  printf 'FAIL: enumerated zero containers in %s\n' "$manifest" >&2
  exit 1
}

is_openbao() {
  local n
  for n in "${openbao_containers[@]}"; do [ "$n" = "$1" ] && return 0; done
  return 1
}

seen_openbao=0
while IFS= read -r name; do
  [ -n "$name" ] || continue
  # Scope the lookup to the container lists. A recursive `..` would also match the Job's own
  # metadata.name, which is likewise "vault-config", and read its (absent) securityContext.
  ctrs='[.spec.template.spec.initContainers[], .spec.template.spec.containers[]]'
  uid="$(yq -e "$ctrs | map(select(.name == \"$name\")) | .[0].securityContext.runAsUser // \"inherit\"" "$manifest")"
  gid="$(yq -e "$ctrs | map(select(.name == \"$name\")) | .[0].securityContext.runAsGroup // \"inherit\"" "$manifest")"
  if is_openbao "$name"; then
    seen_openbao=$((seen_openbao + 1))
    [ "$uid" = "$low_uid" ] || fail "container '$name' must pin the image-baked runAsUser $low_uid (got $uid)"
    [ "$gid" = "$low_gid" ] || fail "container '$name' must pin the image-baked runAsGroup $low_gid (got $gid)"
  else
    [ "$uid" = "inherit" ] || [ "$uid" -ge "$high_id" ] ||
      fail "container '$name' has no image-baked low identity, so it must take the pod's high runAsUser (got $uid)"
    [ "$gid" = "inherit" ] || [ "$gid" -ge "$high_id" ] ||
      fail "container '$name' has no image-baked low identity, so it must take the pod's high runAsGroup (got $gid)"
  fi
done <<EOF
$names
EOF

[ "$seen_openbao" -eq "${#openbao_containers[@]}" ] ||
  fail "expected ${#openbao_containers[@]} openbao containers, matched $seen_openbao — the container names this boundary is written against have changed"

if [ "$status" -eq 0 ]; then
  printf 'PASS: vault-config runs a low identity for exactly %s containers (%s); all others take the high pod defaults\n' \
    "${#openbao_containers[@]}" "$(
      IFS=,
      echo "${openbao_containers[*]}"
    )"
fi
exit "$status"
