#!/usr/bin/env bash
# The legacy candidate is a reviewed historical snapshot, never a live lookup.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sut="$root_dir/scripts/generate-publish-workflow-approved-revisions.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
readonly frozen=ea4df9004d4ab43b6c9af5b0c5f55712f7211328
readonly app_blob=75678d31971b4febd9bbeab4d5d307513ce0fb9d
readonly manifests_blob=59280ec126d1581198b0981766219d296f84d2bd
readonly header=$'repository\tworkflow\ttag\tcommit\tworkflow_blob\tobserved_on'

# Old code gets plausible answers, but the test still fails if it makes ANY
# request. This distinguishes removing the dependency from masking its failure.
cat > "$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FROZEN_TEST_LOG"
case "$*" in
  *releases/latest*) printf '%s\n' v13.10.12 ;;
  *commits/v13.10.12*) printf '%s\n' ea4df9004d4ab43b6c9af5b0c5f55712f7211328 ;;
  *publish-app.yaml*) printf '%s\n' 75678d31971b4febd9bbeab4d5d307513ce0fb9d ;;
  *publish-manifests.yaml*) printf '%s\n' 59280ec126d1581198b0981766219d296f84d2bd ;;
  *) printf '%s\n' 1111111111111111111111111111111111111111 ;;
esac
STUB
chmod +x "$work/bin/gh"

snapshot="$work/valid.tsv"
printf '%s\n' "$header" > "$snapshot"
printf 'devantler-tech/actions\tpublish-app\tv13.10.12\t%s\t%s\t2026-10-20\n' "$frozen" "$app_blob" >> "$snapshot"
printf 'devantler-tech/actions\tpublish-manifests\tv13.10.12\t%s\t%s\t2026-10-20\n' "$frozen" "$manifests_blob" >> "$snapshot"

passed=0
failed=0
run_case() {
  local name="$1" workflow="$2" record="$3" want="$4" output rc=0
  : > "$work/requests.log"
  output="$(env PATH="$work/bin:$PATH" FROZEN_TEST_LOG="$work/requests.log" \
    FROZEN_LEGACY_RELEASE_FILE="$record" bash -c \
    "source \"\$1\"; default_release_resolver \"\$2\"" bash "$sut" "$workflow" 2>"$work/error.log")" || rc=$?
  if [[ -s "$work/requests.log" || ( "$want" == accept && ( "$rc" != 0 || "$output" != "$frozen" ) ) || \
    ( "$want" == reject && ( "$rc" == 0 || -n "$output" ) ) ]]; then
    printf 'FAIL: %s (exit=%s, network_requests=%s)\n' "$name" "$rc" "$(wc -l < "$work/requests.log" | tr -d ' ')"
    failed=$((failed + 1))
  else
    passed=$((passed + 1))
  fi
}

run_case 'app resolves without repository access' publish-app "$snapshot" accept
run_case 'manifests resolve without repository access' publish-manifests "$snapshot" accept
run_case 'unsupported workflow is refused' publish-other "$snapshot" reject
run_case 'missing snapshot is refused' publish-app "$work/missing.tsv" reject

mutate() {
  local name="$1" expression="$2"
  sed "$expression" "$snapshot" > "$work/mutant.tsv"
  run_case "$name" publish-app "$work/mutant.tsv" reject
}
mutate 'wrong header' '1s/repository/repo/'
mutate 'different producer' 's/devantler-tech\/actions/other-org\/actions/'
mutate 'maintained identity cannot become a legacy candidate' 's/devantler-tech\/actions/devantler-tech\/.github/'
mutate 'missing app source' '/publish-app/d'
mutate 'missing manifests source' '/publish-manifests/d'
mutate 'unexpected workflow' 's/publish-manifests/publish-other/'
mutate 'short commit' "s/$frozen/abc123/g"
mutate 'branch instead of commit' "s/$frozen/main/g"
mutate 'uppercase commit' "s/$frozen/EA4DF9004D4AB43B6C9AF5B0C5F55712F7211328/g"
mutate 'short workflow blob' "s/$app_blob/abc123/"
mutate 'missing workflow blob' "s/$app_blob//"
mutate 'mismatched commits' "3s/$frozen/1111111111111111111111111111111111111111/"
mutate 'mismatched tags' '3s/v13.10.12/v13.10.11/'
mutate 'prerelease is not the frozen stable release' 's/v13.10.12/v13.10.12-rc.1/g'
mutate 'branch is not a release tag' 's/v13.10.12/main/g'
mutate 'invalid observation date' 's/2026-10-20/yesterday/g'
mutate 'mismatched observation dates' '3s/2026-10-20/2026-10-19/'
mutate 'extra field' '2s/$/\textra/'
mutate 'missing final field' '2s/\t2026-10-20$//'

cp "$snapshot" "$work/mutant.tsv"
sed -n '2p' "$snapshot" >> "$work/mutant.tsv"
run_case 'duplicate row' publish-app "$work/mutant.tsv" reject
cp "$snapshot" "$work/mutant.tsv"
printf '\n' >> "$work/mutant.tsv"
run_case 'blank extra row' publish-app "$work/mutant.tsv" reject
ln -s "$snapshot" "$work/link.tsv"
run_case 'symlink snapshot' publish-app "$work/link.tsv" reject
run_case 'committed app snapshot' publish-app '' accept
run_case 'committed manifests snapshot' publish-manifests '' accept

# Keep the removed dispatch path absent. Retired paths remain in the CI selector,
# so reintroducing one still executes this production-tree assertion.
retired_paths=(
  .github/workflows/watch-actions-release.yaml
  scripts/watch-actions-release-candidate.sh
  scripts/tests/test-watch-actions-release-candidate.sh
)
for path in "${retired_paths[@]}"; do
  if [[ -e "$root_dir/$path" || -L "$root_dir/$path" ]]; then
    printf 'FAIL: retired release-discovery path remains: %s\n' "$path"
    failed=$((failed + 1))
  else
    passed=$((passed + 1))
  fi
done

# Exercise the real generator entry point too. Only its external publication and
# pin observations are fixtures; consumer discovery and frozen resolution are real.
cat > "$work/observe" <<'STUB'
#!/usr/bin/env bash
printf '1.2.3\tsha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tea4df9004d4ab43b6c9af5b0c5f55712f7211328\n'
STUB
cat > "$work/pin" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' ea4df9004d4ab43b6c9af5b0c5f55712f7211328
STUB
chmod +x "$work/observe" "$work/pin"
printf '{"schemaVersion":1,"publications":[]}\n' > "$work/bootstrap.json"
generate() {
  env PATH="$work/bin:$PATH" FROZEN_TEST_LOG="$work/requests.log" \
    FROZEN_LEGACY_RELEASE_FILE="$1" APPROVED_REVISION_RELEASE_RESOLVER='' \
    APPROVED_REVISION_OBSERVER="$work/observe" APPROVED_REVISION_PIN_RESOLVER="$work/pin" \
    PUBLISH_BOOTSTRAP_FILE="$work/bootstrap.json" APPROVED_REVISIONS_FILE="$work/generated.tsv" \
    bash "$sut" > "$work/generation.log" 2>&1
}
: > "$work/requests.log"
if generate '' && [[ ! -s "$work/requests.log" ]] && \
  awk -F'\t' -v candidate="$frozen" 'NR>1 { if (NF!=8 || $7!=candidate) bad=1; rows++ } END {exit bad || !rows}' "$work/generated.tsv"; then
  passed=$((passed + 1))
else
  printf 'FAIL: complete generator must use the frozen default without network requests\n'
  failed=$((failed + 1))
fi
if [[ -f "$work/generated.tsv" && -f "$work/generated.identities.tsv" ]]; then
  cp "$work/generated.tsv" "$work/generated.before"
  cp "$work/generated.identities.tsv" "$work/identities.before"
  : > "$work/requests.log"
  if generate "$work/missing.tsv" || [[ -s "$work/requests.log" ]] || \
    ! cmp -s "$work/generated.tsv" "$work/generated.before" || \
    ! cmp -s "$work/generated.identities.tsv" "$work/identities.before"; then
    printf 'FAIL: invalid frozen evidence must refuse without changing either output\n'
    failed=$((failed + 1))
  else
    passed=$((passed + 1))
  fi
else
  printf 'FAIL: complete generator did not create both outputs\n'
  failed=$((failed + 1))
fi

printf 'frozen legacy publisher: %s passed, %s failed\n' "$passed" "$failed"
[[ "$failed" == 0 ]]
