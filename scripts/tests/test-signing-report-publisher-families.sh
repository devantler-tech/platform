#!/usr/bin/env bash
# Exercise the report's real default resolver with offline publication evidence.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/report-publish-workflow-signing-revisions.sh
source "$ROOT/scripts/report-publish-workflow-signing-revisions.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# This suite injects the five legacy/dual-family consumers below. Use the same
# explicit universe for the report's registration floor instead of borrowing
# canonical-only registrations from the production tree.
# shellcheck source=scripts/tests/lib/legacy-consumer-fixture.sh
source "$ROOT/scripts/tests/lib/legacy-consumer-fixture.sh"
build_legacy_consumer_fixture "$ROOT" "$WORK/legacy-consumers"
export PUBLISH_CONSUMER_ROOT="$WORK/legacy-consumers"
SHA='1111111111111111111111111111111111111111'
COMMIT='3333333333333333333333333333333333333333'
MAIN_COMMIT='4444444444444444444444444444444444444444'
DIGEST='sha256:5555555555555555555555555555555555555555555555555555555555555555'
fixture_case=canonical-signed
unset PUBLISH_REVISION_RESOLVER

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

discover_consumers() {
  printf '%s\n' \
    $'.github\tpublish-manifests\t1.0.0\tgithub-config/manifests' \
    $'ascoachingogvaner\tpublish-app\t1.0.0\tascoachingogvaner/manifests' \
    $'aws\tpublish-manifests\t1.0.0\taws/manifests' \
    $'data-product-controller\tpublish-app\t1.0.0\tdata-product-controller/manifests' \
    $'wedding-app\tpublish-app\t1.0.0\twedding-app/manifests'
}

deployed_tag() { printf 'v1.0.0\tpinned\t%s\n' "$COMMIT"; }

gh_retry() {
  local args="$*" family=actions workflow=publish-manifests second=''
  if [[ "$args" == *'--jq .default_branch' ]]; then
    printf 'main\n'
    return
  fi
  case "$args" in
    *repos/devantler-tech/wedding-app/commits/main*) printf '%s\n' "$MAIN_COMMIT"; return ;;
    *repos/devantler-tech/wedding-app/commits/v1.0.0*) printf '%s\n' "$COMMIT"; return ;;
    *repos/devantler-tech/business-site/commits/main*) printf '%s\n' "$MAIN_COMMIT"; return ;;
    *repos/devantler-tech/business-site/contents/.github/workflows/publish-runtime.yaml*)
      printf '%s\n' 'jobs:' '  publish:' '    steps:' \
        "      - uses: devantler-tech/.github/actions/publish-app@$SHA"
      return ;;
    *orgs/devantler-tech/packages/container/wedding-app%2Fmanifests/versions*)
      local selected_digest="$DIGEST"
      if [[ "$fixture_case" == own-release-moved && -f "$WORK/verified-digest" ]]; then
        selected_digest='sha256:6666666666666666666666666666666666666666666666666666666666666666'
      fi
      printf '[[{"name":"%s","metadata":{"container":{"tags":["1.0.0"]}}}]]\n' "$selected_digest"
      return ;;
  esac
  [[ "$args" == *'contents/.github/workflows/cd.yaml'* ]] || return 1
  case "$args" in
    *wedding-app*)
      workflow=publish-app
      case "$fixture_case" in
        own-release-transition)
          if [[ "$args" != *"ref=$COMMIT"* ]]; then
            printf '%s\n' 'jobs:' '  publish:' '    steps:' \
              "      - uses: devantler-tech/.github/actions/publish-app@$SHA"
            return
          fi
          ;;
        own-release*)
          printf '%s\n' 'jobs:' '  publish:' '    steps:' \
            "      - uses: devantler-tech/.github/actions/publish-app@$SHA"
          if [[ "$fixture_case" == own-release-mixed ]]; then
            printf '%s\n' "  shared: {uses: devantler-tech/.github/.github/workflows/publish-app.yaml@$SHA}"
          fi
          return ;;
      esac
      ;;
    *ascoachingogvaner* | *data-product-controller*) workflow=publish-app ;;
    *'devantler-tech/.github/'*)
      case "$fixture_case" in
        canonical-signed) [[ "$args" != *ref=main* ]] && family=.github ;;
        canonical-both) family=.github ;;
        ambiguous) second="  second: {uses: devantler-tech/.github/.github/workflows/publish-manifests.yaml@$SHA}" ;;
        duplicate) second="  second: {uses: devantler-tech/actions/.github/workflows/publish-manifests.yaml@$SHA}" ;;
        unknown) family=unregistered ;;
        unknown-mixed) second="  second: {uses: another-owner/actions/.github/workflows/publish-manifests.yaml@$SHA}" ;;
        floating-mixed) second='  second: {uses: devantler-tech/actions/.github/workflows/publish-manifests.yaml@main}' ;;
        multiline) second="  second: {uses: \"devantler-tech/actions/.github/workflows/publish-manifests.yaml@$SHA\\njunk\"}" ;;
        malformed) second='---
bad: [' ;;
        canonical-app)
          family=.github
          workflow=publish-app
          ;;
      esac
      ;;
  esac
  printf 'jobs:\n  publish: {uses: devantler-tech/%s/.github/workflows/%s.yaml@%s}\n%s\n' \
    "$family" "$workflow" "$SHA" "$second"
}

cosign() {
  [[ "$fixture_case" == own-release || "$fixture_case" == own-release-moved ]] || return 1
  [[ "$*" == "verify --certificate-identity https://github.com/devantler-tech/wedding-app/.github/workflows/cd.yaml@refs/tags/v1.0.0 --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-github-workflow-repository devantler-tech/wedding-app --certificate-github-workflow-ref refs/tags/v1.0.0 --certificate-github-workflow-sha $COMMIT ghcr.io/devantler-tech/wedding-app/manifests@$DIGEST" ]] || return 1
  touch "$WORK/verified-digest"
  jq -n --arg digest "$DIGEST" '[{critical: {
    image: {"docker-manifest-digest": $digest},
    identity: {"docker-reference": "ghcr.io/devantler-tech/wedding-app/manifests"}
  }}]'
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

fixture_case=own-release
main >"$WORK/own-release" 2>&1 || fail 'a directly signed release cannot be attributed by the report'
grep -Eq '^OWN-RELEASE +wedding-app .*signed=devantler-tech/wedding-app/.*@refs/tags/v1\.0\.0' \
  "$WORK/own-release" || fail 'report did not name the verified release workflow and tag'

business_current="$(report_current_publisher business-site publish-app main)" ||
  fail 'the direct Business Site publisher cannot be compared with a verified historical binding'
[[ "$business_current" == "devantler-tech/business-site/.github/workflows/publish-runtime.yaml@$MAIN_COMMIT" ]] ||
  fail 'the Business Site publisher used the wrong release workflow'

fixture_case=own-release-transition
main >"$WORK/own-release-transition" 2>&1 || fail 'historical shared signer lost during the transition'
grep -Eq '^TRANSITION +wedding-app .*signed=devantler-tech/actions/.*current=devantler-tech/wedding-app/' \
  "$WORK/own-release-transition" || fail 'the report did not retain both publication and current workflow evidence'

rm -f "$WORK/verified-digest"
for fixture_case in own-release-rejected own-release-moved own-release-mixed; do
  if main >"$WORK/$fixture_case" 2>&1; then fail "$fixture_case was accepted"; fi
  grep -Eq '^UNRESOLVED +wedding-app ' "$WORK/$fixture_case" ||
    fail "$fixture_case refusal lost its consumer"
  printf 'PASS: %s remains unresolved\n' "$fixture_case"
done

for fixture_case in ambiguous duplicate unknown malformed canonical-app unknown-mixed floating-mixed multiline; do
  if main >"$WORK/$fixture_case" 2>&1; then fail "$fixture_case caller was accepted"; fi
  grep -Eq '^UNRESOLVED +[.]github ' "$WORK/$fixture_case" || fail "$fixture_case refusal lost its consumer"
  printf 'PASS: %s caller remains unresolved\n' "$fixture_case"
done
