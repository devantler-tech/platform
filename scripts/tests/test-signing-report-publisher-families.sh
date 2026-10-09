#!/usr/bin/env bash
# Exercise the report's real default resolver with offline publication evidence.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/report-publish-workflow-signing-revisions.sh
source "$ROOT/scripts/report-publish-workflow-signing-revisions.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SHA='1111111111111111111111111111111111111111'
COMMIT='3333333333333333333333333333333333333333'
fixture_case=canonical-signed
unset PUBLISH_REVISION_RESOLVER

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

discover_consumers() {
  printf '%s\n' \
    $'.github\tpublish-manifests\t1.0.0\tgithub-config/manifests' \
    $'ascoachingogvaner\tpublish-app\t1.0.0\tascoachingogvaner/manifests' \
    $'aws\tpublish-manifests\t1.0.0\taws/manifests' \
    $'wedding-app\tpublish-app\t1.0.0\twedding-app/manifests'
}

deployed_tag() { printf 'v1.0.0\tpinned\t%s\n' "$COMMIT"; }

gh_retry() {
  local args="$*" family=actions workflow=publish-manifests second=''
  if [[ "$args" == *'--jq .default_branch' ]]; then printf 'main\n'; return; fi
  [[ "$args" == *'contents/.github/workflows/cd.yaml'* ]] || return 1
  case "$args" in
    *ascoachingogvaner*|*wedding-app*) workflow=publish-app ;;
    *'devantler-tech/.github/'*)
      case "$fixture_case" in
        canonical-signed) [[ "$args" != *ref=main* ]] && family=.github ;;
        canonical-both) family=.github ;;
        ambiguous) second="  second: {uses: devantler-tech/.github/.github/workflows/publish-manifests.yaml@$SHA}" ;;
        unknown) family=unregistered ;;
        unknown-mixed) second="  second: {uses: another-owner/actions/.github/workflows/publish-manifests.yaml@$SHA}" ;;
        floating-mixed) second='  second: {uses: devantler-tech/actions/.github/workflows/publish-manifests.yaml@main}' ;;
        multiline) second="  second: {uses: \"devantler-tech/actions/.github/workflows/publish-manifests.yaml@$SHA\\njunk\"}" ;;
        malformed) second='---
bad: [' ;;
        canonical-app) family=.github; workflow=publish-app ;;
      esac ;;
  esac
  printf 'jobs:\n  publish: {uses: devantler-tech/%s/.github/workflows/%s.yaml@%s}\n%s\n' \
    "$family" "$workflow" "$SHA" "$second"
}

if ! main >"$WORK/canonical-signed" 2>&1; then
  fail 'a published canonical manifest signer cannot be attributed by the report'
fi
grep -Eq '^DIVERGED +[.]github .*signed=devantler-tech/[.]github/.* pinned=devantler-tech/actions/' \
  "$WORK/canonical-signed" || fail 'equal SHAs from different publisher families were reported in sync'
grep -q 'not applied-revision evidence' "$WORK/canonical-signed" || fail 'publication overclaimed production adoption'
printf 'PASS: canonical publication and legacy current pin retain separate identities\n'

fixture_case=canonical-both
main >"$WORK/canonical-both" 2>&1 || fail 'matching canonical identities did not resolve'
grep -Eq '^IN-SYNC +[.]github .*signed=devantler-tech/[.]github/' "$WORK/canonical-both" ||
  fail 'canonical agreement lost its publisher family'
if pin_at_ref .github publish-manifests main >"$WORK/legacy-pin"; then
  fail 'the legacy approval generator accepted a canonical revision as a legacy pin'
fi
printf 'PASS: canonical agreement preserves identity without widening legacy approval generation\n'

fixture_case=legacy
main >"$WORK/legacy" 2>&1 || fail 'retained legacy publishers stopped resolving'
grep -Eq "^IN-SYNC +[.]github .*signed=$SHA pinned=$SHA" "$WORK/legacy" || fail 'legacy report format changed'
printf 'PASS: retained legacy publishers keep their report contract\n'

for fixture_case in ambiguous unknown malformed canonical-app unknown-mixed floating-mixed multiline; do
  if main >"$WORK/$fixture_case" 2>&1; then fail "$fixture_case caller was accepted"; fi
  grep -Eq '^UNRESOLVED +[.]github ' "$WORK/$fixture_case" || fail "$fixture_case refusal lost its consumer"
  printf 'PASS: %s caller remains unresolved\n' "$fixture_case"
done
