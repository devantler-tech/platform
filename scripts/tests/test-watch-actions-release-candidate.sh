#!/usr/bin/env bash
# Contract for scripts/watch-actions-release-candidate.sh (#3960): it dispatches the approved-revision
# regeneration exactly when the committed release candidate is behind the latest actions release and
# no pending regeneration already carries it, and every failed read is UNKNOWN (exit 2, nothing
# dispatched) rather than a quiet "current". A stubbed `gh` answers each read; no network.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sut="${root_dir}/scripts/watch-actions-release-candidate.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail=0

old=1111111111111111111111111111111111111111
new=2222222222222222222222222222222222222222
header=$'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_signer_sha\tmain_pin_sha\trelease_candidate_sha\tobserved_on'

# set_file <path> <candidate for each publish-app row...>
set_file() {
  local path="$1"; shift
  {
    printf '%s\n' "$header"
    printf '.github\tpublish-manifests\t1.0.0\tsha256:x\t%s\t%s\t-\t2026-09-21\n' "$old" "$old"
    local c
    for c in "$@"; do printf 'app\tpublish-app\t1.0.0\tsha256:x\t%s\t%s\t%s\t2026-09-21\n' "$old" "$old" "$c"; done
  } >"$path"
}

mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
  "workflow run"*) exit 0 ;;
  *releases/latest*) [ "${STUB_FAIL:-}" = tag ] && exit 1; printf '%s\t%s\n' "$STUB_TAG" "${STUB_PUBLISHED:-2026-09-21T10:00:00Z}" ;;
  *workflows/regenerate-publish-workflow-approved-revisions.yaml/runs*) [ "${STUB_FAIL:-}" = runs ] && exit 1; printf '%s\n' "${STUB_LAST_RUN:-2026-09-21T06:20:00Z}" ;;
  *actions/commits/*) [ "${STUB_FAIL:-}" = commit ] && exit 1; printf '%s\n' "$STUB_SHA" ;;
  *publish-app.yaml*) [ "${STUB_FAIL:-}" = blob ] && exit 1; printf '%s\n' "${STUB_BLOB:-3333333333333333333333333333333333333333}" ;;
  *matching-refs*) [ "${STUB_FAIL:-}" = refs ] && exit 1; printf '%s\n' "${STUB_REFS:-0}" ;;
  *contents/scripts/publish-workflow-approved-revisions.tsv*) [ "${STUB_FAIL:-}" = pending ] && exit 1; cat "$STUB_PENDING" ;;
  *) echo "unexpected gh call: $*" >&2; exit 99 ;;
esac
STUB
chmod +x "$work/bin/gh"

# case_ <name> <want-exit> <want-output> <want-dispatch yes|no> [env assignments...]
case_() {
  local name="$1" want="$2" needle="$3" dispatch="$4"; shift 4
  local out rc=0 log="$work/$name.log"
  : >"$log"
  out="$(env PATH="$work/bin:$PATH" STUB_LOG="$log" STUB_TAG=v1.2.3 STUB_SHA="$new" \
    STUB_PENDING="$work/pending.tsv" WATCH_DISPATCH=1 GITHUB_REPOSITORY=devantler-tech/platform "$@" \
    bash "$sut" "$work/set.tsv" 2>&1)" || rc=$?
  local dispatched=no
  grep -q '^workflow run regenerate-publish-workflow-approved-revisions.yaml --repo devantler-tech/platform --ref main$' "$log" && dispatched=yes
  if [[ "$rc" != "$want" || "$out" != *"$needle"* || "$dispatched" != "$dispatch" ]]; then
    printf '  FAIL %s: want exit %s, "%s", dispatch=%s; got exit %s, dispatch=%s: %s\n' \
      "$name" "$want" "$needle" "$dispatch" "$rc" "$dispatched" "$out"
    fail=1
  else
    printf '  ok   %s\n' "$name"
  fi
}

set_file "$work/set.tsv" "$old" "$old"
set_file "$work/pending.tsv" "$old" "$old"
case_ behind-dispatches 0 'DISPATCHED' yes
case_ dry-run-dispatches-nothing 0 'dry run' no WATCH_DISPATCH=
case_ stale-pending-branch-still-dispatches 0 'DISPATCHED' yes STUB_REFS=1

set_file "$work/pending.tsv" "$new" "$new"
case_ pending-branch-carries-it 0 'PENDING' no STUB_REFS=1

set_file "$work/set.tsv" "$new" "$new"
case_ current-set-dispatches-nothing 0 'CURRENT' no

# One row behind is still behind: the candidate is shared, so a partial set is not current.
set_file "$work/set.tsv" "$new" "$old"
case_ one-row-behind-dispatches 0 'DISPATCHED' yes

# A regeneration that started after the release has had its chance; a repository that never ran
# one still dispatches.
case_ regeneration-already-ran-since-release 0 'ALREADY-RAN' no STUB_LAST_RUN=2026-09-21T10:30:00Z
case_ never-regenerated-dispatches 0 'DISPATCHED' yes STUB_LAST_RUN=none

set_file "$work/set.tsv"
case_ no-publish-app-consumer 0 'nothing to keep current' no

# Every failed or implausible read is UNKNOWN and dispatches nothing.
set_file "$work/set.tsv" "$old"
case_ tag-read-fails 2 'UNKNOWN' no STUB_FAIL=tag
case_ implausible-tag 2 'implausible release tag' no 'STUB_TAG=v1;rm'
case_ commit-read-fails 2 'UNKNOWN' no STUB_FAIL=commit
case_ short-commit 2 'not one commit' no STUB_SHA=abc123
case_ release-without-publish-app 2 'does not carry publish-app.yaml' no STUB_FAIL=blob
case_ refs-read-fails 2 'UNKNOWN' no STUB_FAIL=refs
case_ pending-read-fails 2 'UNKNOWN' no STUB_FAIL=pending STUB_REFS=1
case_ runs-read-fails 2 'UNKNOWN' no STUB_FAIL=runs
case_ implausible-release-time 2 'implausible release time' no STUB_PUBLISHED=yesterday
case_ implausible-run-time 2 'implausible regeneration run time' no STUB_LAST_RUN=soon

printf 'not-the-header\n' >"$work/set.tsv"
case_ reshaped-set-is-unknown 2 'approved-set header' no

if [[ "$fail" != 0 ]]; then exit 1; fi
echo 'PASS: watch-actions-release-candidate dispatches only when the candidate is behind and nothing pending carries it'
