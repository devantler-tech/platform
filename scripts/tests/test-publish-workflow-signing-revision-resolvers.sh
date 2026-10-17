#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR
# Run the real resolvers with strict, local GitHub responses; mutation controls
# prove every regression assertion rejects its corresponding broken resolver.
set -euo pipefail
test_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_source="${test_root}/scripts/report-publish-workflow-signing-revisions.sh"

assert_equal() {
  [[ "$1" == "$2" ]] || { printf 'Expected %q, got %q\n' "$1" "$2" >&2; exit 1; }
}

if [[ "${1:-}" == --case ]]; then
  test_case="$2"
  # shellcheck source=../report-publish-workflow-signing-revisions.sh
  source "$3"
  for resolver in pin_at_ref tag_was_published deployed_tag; do
    declare -F "${resolver}" >/dev/null || { echo "Missing resolver: ${resolver}" >&2; exit 1; }
  done
  case "${test_case}" in
    comment|same)
      assert_equal "${SIGNING_TEST_PIN}" "$(pin_at_ref fixture publish-app main)"
      ;;
    different|malformed)
      if pin_at_ref fixture publish-app main >"${SIGNING_TEST_FIXTURE}/answer"; then
        echo 'Ambiguous or malformed workflow pins were accepted' >&2; exit 1
      fi
      [[ ! -s "${SIGNING_TEST_FIXTURE}/answer" ]]
      ;;
    failed)
      test_status=0
      tag_was_published fixture v1.2.3 "${SIGNING_TEST_COMMIT}" || test_status=$?
      assert_equal 1 "${test_status}"
      ;;
    unpublished)
      if deployed_tag fixture 1.2.3 >"${SIGNING_TEST_FIXTURE}/answer"; then
        echo 'An unpublished exact tag resolved' >&2; exit 1
      fi
      [[ ! -s "${SIGNING_TEST_FIXTURE}/answer" ]]
      ;;
    healthy)
      assert_equal $'v1.2.3\tpinned\t'"${SIGNING_TEST_COMMIT}" "$(deployed_tag fixture 1.2.3)"
      ;;
    *) echo 'Unknown fixture case' >&2; exit 1 ;;
  esac
  [[ ! -s "${SIGNING_TEST_FIXTURE}/unexpected" ]] || { cat "${SIGNING_TEST_FIXTURE}/unexpected" >&2; exit 1; }
  exit 0
fi

test_work="$(mktemp -d)"
trap 'rm -rf "${test_work}"' EXIT
mkdir -p "${test_work}/bin"
cat >"${test_work}/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
request=""
reject() {
  printf 'Unexpected GitHub request: %s\n' "$*" >>"${SIGNING_TEST_FIXTURE}/unexpected"
  exit 91
}
for argument in "$@"; do
  case "${argument}" in repos/*|orgs/*) request="${argument}" ;; esac
done
printf '%s\n' "$*" >>"${SIGNING_TEST_FIXTURE}/requests"
case "${request}" in
  repos/devantler-tech/fixture/contents/.github/workflows/cd.yaml)
    [[ "$*" == 'api --method GET repos/devantler-tech/fixture/contents/.github/workflows/cd.yaml --raw-field ref=main -H Accept: application/vnd.github.raw' ]] || reject "$@"
    cat "${SIGNING_TEST_FIXTURE}/workflow"
    ;;
  repos/devantler-tech/fixture/actions/runs)
    [[ "$*" == 'api --method GET repos/devantler-tech/fixture/actions/runs --raw-field branch=v1.2.3 --raw-field event=push --raw-field per_page=100' ]] || reject "$@"
    cat "${SIGNING_TEST_FIXTURE}/runs"
    ;;
  'repos/devantler-tech/fixture/tags?per_page=100')
    [[ "$*" == 'api --paginate repos/devantler-tech/fixture/tags?per_page=100 --jq .[].name' ]] || reject "$@"
    printf 'v1.2.3\n'
    ;;
  repos/devantler-tech/fixture/commits/v1.2.3)
    [[ "$*" == 'api repos/devantler-tech/fixture/commits/v1.2.3 --jq .sha' ]] || reject "$@"
    printf '%s\n' "${SIGNING_TEST_COMMIT}"
    ;;
  'orgs/devantler-tech/packages/container/fixture%2Fmanifests/versions?per_page=100')
    [[ "$*" == 'api --paginate orgs/devantler-tech/packages/container/fixture%2Fmanifests/versions?per_page=100 --jq .[].metadata.container.tags[]?' ]] || reject "$@"
    printf '1.2.3\n'
    ;;
  *) reject "$@" ;;
esac
STUB
# Retries are exercised without waiting on wall-clock time.
printf '#!/usr/bin/env bash\nexit 0\n' >"${test_work}/bin/sleep"
chmod +x "${test_work}/bin/gh" "${test_work}/bin/sleep"
export PATH="${test_work}/bin:${PATH}"
export SIGNING_TEST_PIN=2222222222222222222222222222222222222222
export SIGNING_TEST_COMMIT=3333333333333333333333333333333333333333

make_fixture() {
  export SIGNING_TEST_FIXTURE="${test_work}/$1-$2"
  mkdir -p "${SIGNING_TEST_FIXTURE}"
  local extra="" conclusion=success
  case "$1" in
    comment) extra='  # old:
  #   uses: devantler-tech/actions/.github/workflows/publish-app.yaml@1111111111111111111111111111111111111111' ;;
    different) extra='  other:
    uses: devantler-tech/actions/.github/workflows/publish-app.yaml@1111111111111111111111111111111111111111' ;;
    same) extra="  other:
    uses: devantler-tech/actions/.github/workflows/publish-app.yaml@${SIGNING_TEST_PIN}" ;;
    failed|unpublished) conclusion=failure ;;
  esac
  printf 'name: CD\njobs:\n%s\n  publish:\n    uses: devantler-tech/actions/.github/workflows/publish-app.yaml@%s\n' \
    "${extra}" "${SIGNING_TEST_PIN}" >"${SIGNING_TEST_FIXTURE}/workflow"
  if [[ "$1" == malformed ]]; then
    printf '\n---\njobs: [\n' >>"${SIGNING_TEST_FIXTURE}/workflow"
  fi
  jq -n --arg sha "${SIGNING_TEST_COMMIT}" --arg conclusion "${conclusion}" \
    '{total_count:1,workflow_runs:[{head_branch:"v1.2.3",path:".github/workflows/cd.yaml",head_sha:$sha,conclusion:$conclusion}]}' \
    >"${SIGNING_TEST_FIXTURE}/runs"
}

for test_case in malformed comment different same failed unpublished healthy; do
  make_fixture "${test_case}" real
  bash "${BASH_SOURCE[0]}" --case "${test_case}" "${test_source}"
  printf 'PASS: %s resolver case\n' "${test_case}"

  # Each replacement is scoped to the real helper and must match exactly once.
  # The failed-run control changes both success predicates in its classification.
  mutant="${test_work}/mutant-${test_case}.sh"
  awk -v mutation="${test_case}" '
    /^pin_at_ref\(\)/ { helper="pin" }
    /^tag_was_published\(\)/ { helper="published" }
    /^deployed_tag\(\)/ { helper="deployed" }
    {
      if (helper=="pin" && mutation=="malformed" && /calls=.*yq eval -r/) {
        sub(/\|\| return 1$/, "|| true"); changed++
      }
      if (helper=="pin" && mutation=="comment" && /yq eval -r/) {
        sub(/yq eval -r .* - 2>\/dev\/null/, "sed -n \047s/.*uses: *//p\047"); changed++
      }
      if (helper=="pin" && mutation=="different" && /\[ "\$count" -eq 1 \]/) {
        sub(/-eq 1/, "-ge 1"); changed++
      }
      if (helper=="pin" && mutation=="same" && /sort -u \|\| true/) {
        sub(/sort -u/, "cat"); changed++
      }
      if (helper=="published" && mutation=="failed" && /and \.conclusion == "success"/) {
        sub(/ and \.conclusion == "success"/, ""); changed++
      }
      if (helper=="deployed" && mutation=="unpublished" && /\[ "\$exact_pub" -eq 0 \] \|\| return 1/) {
        sub(/\[ "\$exact_pub" -eq 0 \] \|\| return 1/, ":"); changed++
      }
      if (helper=="deployed" && mutation=="healthy" && !changed && /if ! printf.*registry_tags.*registry_version/) {
        sub(/if ! printf/, "if printf"); changed++
      }
      print
      if (/^}/) helper=""
    }
    END { if (changed != (mutation=="failed" ? 2 : 1)) exit 1 }
  ' "${test_source}" >"${mutant}"
  cmp -s "${test_source}" "${mutant}" && { echo 'Mutation did not change source' >&2; exit 1; }
  make_fixture "${test_case}" mutant
  if bash "${BASH_SOURCE[0]}" --case "${test_case}" "${mutant}" >"${test_work}/mutation-output" 2>&1; then
    echo "Mutation survived: ${test_case}" >&2; exit 1
  fi
  # A malformed mutant or an unexpected request is not a killed regression.
  bash -n "${mutant}"
  [[ ! -s "${SIGNING_TEST_FIXTURE}/unexpected" ]] || { cat "${SIGNING_TEST_FIXTURE}/unexpected" >&2; exit 1; }
  printf 'PASS: applied %s mutation is detected\n' "${test_case}"
done
