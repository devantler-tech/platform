#!/usr/bin/env bash
# The KSV-0020/KSV-0021 dispositions on the private-mail StatefulSet are PATH-scoped, and this test
# is what keeps them honest.
#
# WHY THIS EXISTS
# Stalwart runs at the identity its own image defines: the pinned digest's config User is
# `stalwart`, and its layers carry stalwart:x:2000:2000 in /etc/passwd and stalwart:x:2000 in
# /etc/group. The platform bootstrap image is built FROM that digest and ends with USER 2000:2000,
# and the mail store on the data volume is owned by that identity. trivy reads each file alone and
# scopes an ignore by path, so its entry silences the checks for every container in these files —
# including one added later at some other low identity with no such reason.
#
# Three layers, which fail for different reasons:
#
#   STRUCTURE (always) — each check has exactly one entry, scoped to exactly these three files.
#   PREMISE (always) — every container's effective UID and GID is 2000, the pod default is that
#     identity, and the images are still the measured Stalwart digest and the bootstrap image built
#     from it with USER 2000:2000.
#   BEHAVIOUR (when trivy is installed) — the StatefulSet's bytes at an unlisted probe path must
#     still report both checks with the ignorefile, while the dispositioned path reports neither.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly IGNOREFILE="$REPO_ROOT/.trivyignore.yaml"
readonly SET_PATH='k8s/bases/apps/private-mail/stateful-set.yaml'
readonly SERVE_PATH='k8s/bases/apps/private-mail-administration/patches/serve-after-initialization.yaml'
readonly PROXY_PATH='k8s/bases/apps/private-mail-proxy-identity/patches/add-client-proxy.yaml'
readonly DOCKERFILE='scripts/private-mail-config/Dockerfile'
readonly PROBE_PATH='k8s/bases/apps/trivyignore-private-mail-probe/stateful-set.yaml'
EXPECTED_PATHS="$(printf '%s\n' "$SET_PATH" "$SERVE_PATH" "$PROXY_PATH" | LC_ALL=C sort)"
readonly EXPECTED_PATHS
# The identity was measured from this digest, not from the repository name or tag.
readonly STALWART_IMAGE='ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c'
readonly BOOTSTRAP_REPOSITORY='ghcr.io/devantler-tech/platform-private-mail-bootstrap'
readonly IMAGE_ID=2000

status=0
fail() {
  printf '%s\n' "$*" >&2
  status=1
}

command -v yq >/dev/null 2>&1 || {
  printf 'yq is required to check the private-mail identity disposition boundary\n' >&2
  exit 1
}
for path in "$SET_PATH" "$SERVE_PATH" "$PROXY_PATH" "$DOCKERFILE" .trivyignore.yaml; do
  [ -r "$REPO_ROOT/$path" ] || {
    printf 'not readable: %s\n' "$path" >&2
    exit 1
  }
done

# ---------------------------------------------------------------- structure --
for check in KSV-0020 KSV-0021; do
  entries="$(SET="$SET_PATH" CHECK="$check" yq '[.misconfigurations[] | select(.paths // [] | contains([strenv(SET)]))
    | select(.id == strenv(CHECK))] | length' "$IGNOREFILE" 2>/dev/null)" || entries=''
  if [ "$entries" != 1 ]; then
    fail "STRUCTURE: $check must have exactly one entry naming $SET_PATH, found ${entries:-unreadable}"
    continue
  fi
  paths="$(SET="$SET_PATH" CHECK="$check" yq -r '.misconfigurations[] | select(.paths // [] | contains([strenv(SET)]))
    | select(.id == strenv(CHECK)) | .paths[]' "$IGNOREFILE" | LC_ALL=C sort)"
  [ "$paths" = "$EXPECTED_PATHS" ] || fail "STRUCTURE: the $check entry is scoped to [$paths], not the three reviewed files"
done

# ------------------------------------------------------------------ premise --
# Effective identity per container: the container's own value, else the pod's. The add-client-proxy
# fragment has no pod securityContext; its pod is serve-after-initialization's, so that is its
# default.
pod_value() { yq -r ".spec.template.spec.securityContext.$2 // \"\"" "$REPO_ROOT/$1"; }
for field in runAsUser runAsGroup; do
  for file in "$SET_PATH" "$SERVE_PATH"; do
    [ "$(pod_value "$file" "$field")" = "$IMAGE_ID" ] || fail "PREMISE: $file pod $field is not $IMAGE_ID"
  done
  for file in "$SET_PATH" "$SERVE_PATH" "$PROXY_PATH"; do
    default="$(pod_value "$file" "$field")"
    [ -n "$default" ] || default="$(pod_value "$SERVE_PATH" "$field")"
    while IFS='|' read -r name value; do
      [ -n "$name" ] || continue
      [ "${value:-$default}" = "$IMAGE_ID" ] ||
        fail "PREMISE: container $name in $file runs with $field ${value:-$default}, not the image-defined $IMAGE_ID"
    done < <(yq -r "[(.spec.template.spec.initContainers // [])[], (.spec.template.spec.containers // [])[]]
      | .[] | .name + \"|\" + ((.securityContext.$field // \"\") | tostring)" "$REPO_ROOT/$file")
  done
done

# Every container, not only the main one: a sidecar at the pod's default 2000 would otherwise
# inherit the suppression without running an image that defines that identity.
for file in "$SET_PATH" "$SERVE_PATH" "$PROXY_PATH"; do
  expected="$BOOTSTRAP_REPOSITORY"
  [ "$file" != "$SET_PATH" ] || expected="$STALWART_IMAGE"
  images="$(yq -r '[(.spec.template.spec.initContainers // [])[], (.spec.template.spec.containers // [])[]] | .[].image' "$REPO_ROOT/$file")" ||
    images=''
  [ -n "$images" ] || fail "PREMISE: read no container image from $file"
  while IFS= read -r image; do
    [ -n "$image" ] || continue
    [ "$image" = "$expected" ] || fail "PREMISE: $file runs $image, not $expected, whose image-defined identity was measured"
  done <<<"$images"
done
[ "$(awk 'toupper($1) == "FROM" {base = $2} END {print base}' "$REPO_ROOT/$DOCKERFILE")" = "$STALWART_IMAGE" ] ||
  fail "PREMISE: the bootstrap image's final stage is no longer built from the measured Stalwart digest"
[ "$(awk 'toupper($1) == "USER" {user = $2} END {print user}' "$REPO_ROOT/$DOCKERFILE")" = "$IMAGE_ID:$IMAGE_ID" ] ||
  fail "PREMISE: the bootstrap image no longer ends with USER $IMAGE_ID:$IMAGE_ID"

[ "$status" -eq 0 ] || exit 1
echo "PASS(structural): KSV-0020/KSV-0021 are scoped to the three private-mail files, and every container there runs as $IMAGE_ID:$IMAGE_ID."

# ---------------------------------------------------------------- behaviour --
command -v trivy >/dev/null 2>&1 || {
  echo "SKIP(behavioural): trivy not installed; structural and premise checks above still passed"
  exit 0
}
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/$(dirname "$SET_PATH")" "$work/$(dirname "$PROBE_PATH")" "$work/.trivy"
cp "$REPO_ROOT/$SET_PATH" "$work/$SET_PATH"
cp "$REPO_ROOT/$SET_PATH" "$work/$PROBE_PATH"
cp "$IGNOREFILE" "$work/.trivyignore.yaml"
cp -R "$REPO_ROOT/.trivy/data" "$work/.trivy/data"
(cd "$work" && trivy fs --scanners misconfig --config-data .trivy/data --ignorefile .trivyignore.yaml \
  --format json .) >"$work/out.json" 2>/dev/null
count_at() {
  jq -r --arg t "$1" --arg id "$2" \
    '[ .Results[]? | select(.Target == $t) | .Misconfigurations[]? | select(.ID == $id and .Status == "FAIL") ] | length' \
    "$work/out.json"
}
for check in KSV-0020 KSV-0021; do
  [ "$(count_at "$PROBE_PATH" "$check")" -gt 0 ] ||
    fail "VACUOUS OR BREACHED: $check does not report the same bytes at the unlisted $PROBE_PATH"
  [ "$(count_at "$SET_PATH" "$check")" -eq 0 ] || fail "UNCOVERED: $check still reports $SET_PATH"
done
[ "$status" -eq 0 ] || exit 1
echo "PASS(behavioural): identical bytes report at the probe path and are suppressed only at the reviewed path."
