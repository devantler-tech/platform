#!/usr/bin/env bash
# Guard the default-off release-recovery workflow: consent, scope, immutable source, read token
# before writer token, and required CI coverage of both flag states. Executes the shipped steps.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
workflow="${1:-$root/.github/workflows/publish-release-recovery.yaml}"
ci="${2:-$root/.github/workflows/ci.yaml}"
work="$(mktemp -d)"
work="$(CDPATH='' cd -- "$work" && pwd -P)"
trap 'rm -rf "$work"' EXIT
fail() {
  printf 'release recovery: %s\n' "$1" >&2
  exit 1
}
[[ -f "$workflow" ]] || fail 'recovery workflow is missing'
yq -o=json '.' "$workflow" >"$work/workflow.json"
yq -o=json '.' "$ci" >"$work/ci.json"
jq -n --slurpfile w "$work/workflow.json" --slurpfile c "$work/ci.json" '{w:$w[0],c:$c[0]}' >"$work/bundle.json"
cat >"$work/guard.jq" <<'JQ'
  .w as $w | .c as $c | $w.jobs.recover as $j | $j.steps as $s | $w.jobs["offline-test"] as $o |
  def ordinary: .if == null and .["continue-on-error"] == null;
  def recoveryContractRun: .run == "shellcheck scripts/generate-release-recovery-fixture.sh scripts/tests/test-release-recovery.sh\nbash scripts/tests/test-release-recovery.sh\n";
  def writer: .if == "steps.read-admission.outputs.missing == 'true'" and .["continue-on-error"] == null;
  def source: .env == {CATALOGUE_REPOSITORY:"devantler-tech/.github",CATALOGUE_SHA:"0b137510d139bbf62e479d9610b0db17c972bc2b"} and
    (.run | contains("[[ \"$CATALOGUE_REPOSITORY\" == devantler-tech/.github && \"$CATALOGUE_SHA\" =~ ^[0-9a-f]{40}$ ]]")) and
    (.run | contains("env -i PATH=\"$PATH\" curl --disable")) and
    (.run | contains("https://codeload.github.com/$CATALOGUE_REPOSITORY/tar.gz/$CATALOGUE_SHA")) and
    (.run | contains("cmd/publish-release-recovery/main.go"));
  def build: .env == {GOWORK:"off",GOFLAGS:"",GOTOOLCHAIN:"local",CGO_ENABLED:"0"} and
    (.run | contains("build -trimpath -o \"$PWD/.devantler-tech-recovery/publish-release-recovery\" ./cmd/publish-release-recovery"));
  def incident: {REPOSITORY:"${{ github.repository }}",TAG:"${{ inputs.tag }}",SOURCE_SHA:"${{ inputs.source-sha }}",RUN_ID:"${{ inputs.run-id }}",RUN_ATTEMPT:"${{ inputs.run-attempt }}",PRODUCER_REF:"${{ inputs.producer-ref }}"};
  def token($contents): {"client-id":"${{ vars.APP_CLIENT_ID }}","private-key":"${{ secrets.APP_PRIVATE_KEY }}",owner:"devantler-tech",repositories:"${{ steps.admit.outputs.name }}","permission-contents":$contents,"permission-actions":"read"};
  ($w.on | keys) == ["workflow_dispatch"] and $w.permissions == {} and
  $w.on.workflow_dispatch.inputs["enable-recovery"] == {description:$w.on.workflow_dispatch.inputs["enable-recovery"].description,type:"boolean",default:false} and
  $w.on.workflow_dispatch.inputs["confirm-publication"].type == "boolean" and $w.on.workflow_dispatch.inputs["confirm-publication"].default == false and
  $w.on.workflow_dispatch.inputs["offline-test"].default == false and
  ($w.on.workflow_dispatch.inputs | has("repository") | not) and
  $w.on.workflow_dispatch.secrets == null and
  ($w.jobs | keys) == ["offline-test","recover"] and
  $j.if == "inputs.enable-recovery && !inputs.offline-test" and $j.permissions == {} and $j.needs == null and
  $j.concurrency == {group:"create-release-${{ github.repository }}-${{ github.ref }}","cancel-in-progress":false,queue:"max"} and
  ($s | length == 10) and ($s[0:7] | all(ordinary)) and ($s[7:10] | all(writer)) and
  ($s[0].uses | test("^step-security/harden-runner@[a-f0-9]{40}$")) and $s[0].with["egress-policy"] == "audit" and
  $s[1].id == "admit" and
  $s[1].env == {EVENT_NAME:"${{ github.event_name }}",CONFIRMATION:"${{ inputs.confirm-publication }}",REPOSITORY:"${{ github.repository }}",PRIVATE:"${{ github.event.repository.private }}",CURRENT_REF:"${{ github.ref }}",DEFAULT_BRANCH:"${{ github.event.repository.default_branch }}"} and
  ($s[2] | source) and
  ($s[3].uses | test("^actions/setup-go@[a-f0-9]{40}$")) and $s[3].with.cache == false and
  ($s[4] | build) and
  ($s[5].uses | test("^actions/create-github-app-token@[a-f0-9]{40}$")) and $s[5].id == "read-token" and $s[5].with == token("read") and
  $s[6].id == "read-admission" and $s[6].env == ({GH_TOKEN:"${{ steps.read-token.outputs.token }}"} + incident) and
  ($s[6].run | contains("\"./.devantler-tech-recovery/inspect-release-recovery\"")) and
  $s[7].id == "read-only-refusal" and $s[7].uses == null and $s[7].with == null and $s[7].shell == "bash" and
  $s[7].env == {GH_TOKEN:"${{ steps.read-token.outputs.token }}",REPOSITORY:"${{ github.repository }}",DEFAULT_BRANCH:"${{ github.event.repository.default_branch }}"} and
  ($s[7].run | contains("env -i PATH=\"$PATH\" curl --disable")) and
  ($s[7].run | contains("https://api.github.com/repos/$REPOSITORY/releases/generate-notes")) and
  ($s[8].uses | test("^actions/create-github-app-token@[a-f0-9]{40}$")) and $s[8].id == "write-token" and $s[8].with == token("write") and
  $s[9].id == "publish" and $s[9].env == ({GH_TOKEN:"${{ steps.write-token.outputs.token }}"} + incident) and
  ($s[9].run | contains("\"./.devantler-tech-recovery/publish-release-recovery\"")) and
  ([$s[] | select(.env.GH_TOKEN != null) | .id] == ["read-admission","read-only-refusal","publish"]) and
  $o.if == "inputs.enable-recovery && inputs.offline-test" and $o.permissions == {} and
  ($o.steps | length == 5 and all(ordinary)) and
  ($o.steps[0].uses | test("^step-security/harden-runner@[a-f0-9]{40}$")) and
  ($o.steps[1] | source) and ($o.steps[3] | build) and
  ([$o.steps[] | tostring | select(test("secrets\\.|outputs\\.token|github\\.token"))] | length == 0) and ([$o.steps[] | select(.env.GH_TOKEN != null)] | length == 0) and
  ($o.steps[4].run | contains("unset GH_TOKEN GITHUB_TOKEN")) and
  $c.jobs["test-publish-release-recovery"] == ($o|del(.if)|.name="🩹 Validate the release recovery build") and
  ($c.jobs["test-publish-release-recovery-disabled"] | .if=="github.event_name == 'pull_request'" and .permissions=={contents:"read"} and .["runs-on"]=="ubuntu-latest" and .secrets==null and .uses==null and .["continue-on-error"]==null and (.steps|length)==2 and (.steps|all(ordinary)) and (.steps[0].uses | test("^actions/checkout@[a-f0-9]{40}$")) and .steps[0].with["persist-credentials"]==false and .steps[1].with==null and (.steps[1] | recoveryContractRun)) and
  (["test-publish-release-recovery","test-publish-release-recovery-disabled"] | all(. as $job |
    ($c.jobs["ci-required-checks"].needs | index($job) != null) and
    ($c.jobs["ci-required-checks"].steps | any((.with["job-results"] // "") | contains("needs." + $job + ".result"))))) and
  $c.jobs["test-publish-release-recovery-disabled"].if == "github.event_name == 'pull_request'" and
  ($c.jobs["deploy-prod"].needs | index("test-publish-release-recovery") != null) and
  ($c.jobs["validate-repository-guards"].steps | any(recoveryContractRun and ordinary))
JQ
jq -e -f "$work/guard.jq" "$work/bundle.json" >/dev/null || fail 'workflow authority, provenance or required coverage is unsafe'
shellcheck "$root/scripts/tests/test-release-writer-refusal.sh"
bash "$root/scripts/tests/test-release-writer-refusal.sh" "$workflow"
# An ordinary immutable checkout update must not require changing this guard.
jq '.c.jobs["test-publish-release-recovery-disabled"].steps[0].uses="actions/checkout@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' "$work/bundle.json" >"$work/checkout-update.json"
jq -e -f "$work/guard.jq" "$work/checkout-update.json" >/dev/null || fail 'a SHA-pinned checkout update was refused'
# Exercise the generator too; the independent parsed equality above is the oracle.
cp "$ci" "$work/regenerated.yaml"
bash "$root/scripts/generate-release-recovery-fixture.sh" "$workflow" "$work/regenerated.yaml"
yq -o=json '.' "$work/regenerated.yaml" | jq -S . >"$work/regenerated.json"
jq -S . "$work/ci.json" >"$work/checked.json"
cmp -s "$work/regenerated.json" "$work/checked.json" || fail 'internal recovery fixture needs regeneration'
# Evaluate the shipped, strictly admitted offline condition in all four boolean
# states. This runs the default-off test, rather than skipping its CI job.
offline_condition="$(yq -r '.jobs["offline-test"].if' "$workflow")"
offline_enabled() {
  local expression="${offline_condition//inputs.enable-recovery/$1}"
  expression="${expression//inputs.offline-test/$2}"
  bash -euo pipefail -c "$expression"
}
offline_enabled true true || fail 'explicitly enabled offline condition refused'
for pair in 'false false' 'false true' 'true false'; do
  # shellcheck disable=SC2086 # Split the fixed boolean pair, never repository text.
  if offline_enabled $pair; then fail "offline condition admitted disabled flags: $pair"; fi
done
enable_default="$(yq -r '.on.workflow_dispatch.inputs["enable-recovery"].default' "$workflow")"
offline_default="$(yq -r '.on.workflow_dispatch.inputs["offline-test"].default' "$workflow")"
if offline_enabled "$enable_default" "$offline_default"; then fail 'default flags started the offline job'; fi
# shellcheck disable=SC2016 # Keep GitHub expressions literal in refusal controls.
for mutation in \
  '.w.on.workflow_call={}' \
  '.w.on.workflow_dispatch.inputs["enable-recovery"].default=true' \
  '.w.on.workflow_dispatch.inputs["confirm-publication"].default=true' \
  '.w.on.workflow_dispatch.inputs.repository={type:"string"}' \
  '.w.jobs.recover.if="inputs.enable-recovery"' \
  '.w.jobs.recover.permissions.contents="write"' \
  '.w.jobs.recover.concurrency["cancel-in-progress"]=true' \
  '.w.jobs.recover.concurrency.group="recovery-${{ github.repository }}"' \
  '.w.jobs.recover.steps[1].if="false"' \
  '.w.jobs.recover.steps[1].env.CONFIRMATION="true"' \
  '.w.jobs.recover.steps[2].env.CATALOGUE_SHA="main"' \
  '.w.jobs.recover.steps[3].with.cache=true' \
  '.w.jobs.recover.steps[4].env.GOFLAGS="-toolexec=untrusted"' \
  '.w.jobs.recover.steps |= [.[0],.[1],.[2],.[3],.[5],.[4],.[6],.[7],.[8],.[9]]' \
  '.w.jobs.recover.steps |= [.[0],.[1],.[2],.[3],.[4],.[8],.[6],.[7],.[5],.[9]]' \
  '.w.jobs.recover.steps[5].with["permission-contents"]="write"' \
  '.w.jobs.recover.steps[5].with.repositories=null' \
  '.w.jobs.recover.steps[6].env.GH_TOKEN="${{ steps.write-token.outputs.token }}"' \
  '.w.jobs.recover.steps[7].if=null' \
  '.w.jobs.recover.steps[7].env.GH_TOKEN="${{ steps.write-token.outputs.token }}"' \
  '.w.jobs.recover.steps[7].env.DEFAULT_BRANCH="${{ inputs.tag }}"' \
  '.w.jobs.recover.steps[7].if="false"' \
  '.w.jobs.recover.steps[7]["continue-on-error"]=true' \
  '.w.jobs.recover.steps |= map(select(.id != "read-only-refusal"))' \
  '.w.jobs.recover.steps[8].if=null' \
  '.w.jobs.recover.steps[8].with["permission-issues"]="write"' \
  '.w.jobs.recover.steps[8].with.repositories=null' \
  '.w.jobs.recover.steps[9].env.GH_TOKEN="${{ github.token }}"' \
  '.w.jobs.recover.steps[9].env.REPOSITORY="${{ inputs.tag }}"' \
  '.w.jobs["offline-test"].if="inputs.offline-test"' \
  '.w.jobs["offline-test"].steps[4].env={GH_TOKEN:"${{ secrets.APP_PRIVATE_KEY }}"}' \
  '.c.jobs["test-publish-release-recovery"].if="false"' \
  '.c.jobs["test-publish-release-recovery-disabled"].if="false"' \
  '.c.jobs["deploy-prod"].needs |= map(select(. != "test-publish-release-recovery"))' \
  '.c.jobs["test-publish-release-recovery"].secrets="inherit"' \
  '.c.jobs["test-publish-release-recovery"].steps[1].if="false"' \
  '.c.jobs["test-publish-release-recovery"].steps[1]["continue-on-error"]=true' \
  '.c.jobs["test-publish-release-recovery"].steps[0].with["egress-policy"]="block"' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[1].if="false"' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[0].uses="actions/checkout@v6"' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[0].uses="actions/checkout@bbbbbbbb"' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[0].uses="actions/checkout@BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[0].uses="other/checkout@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
  '.c.jobs["test-publish-release-recovery"].steps[1].with["offline-test"]=false' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[1].with["enable-recovery"]=true' \
  '.c.jobs["ci-required-checks"].needs |= map(select(. != "test-publish-release-recovery"))' \
  '.c.jobs["ci-required-checks"].steps |= map(if .with["job-results"] then .with["job-results"] |= sub("needs.test-publish-release-recovery-disabled.result"; "") else . end)' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[1].run="bash scripts/tests/test-release-recovery.sh"' \
  '.c.jobs["test-publish-release-recovery-disabled"].steps[1].run="shellcheck scripts/tests/test-release-recovery.sh\nbash scripts/tests/test-release-recovery.sh\n"' \
  '.c.jobs["validate-repository-guards"].steps |= map(if .name == "🩹 Validate the release recovery boundary" then .run="bash scripts/tests/test-release-recovery.sh" else . end)' \
  '.c.jobs["validate-repository-guards"].steps |= map(select(.name != "🩹 Validate the release recovery boundary"))' \
  '.c.jobs["test-publish-release-recovery"].uses="./.github/workflows/publish-release-recovery.yaml"'; do
  jq "$mutation" "$work/bundle.json" >"$work/mutated.json"
  if jq -e -f "$work/guard.jq" "$work/mutated.json" >/dev/null; then fail "accepted mutation: $mutation"; fi
done
# Run the shipped admission block: only a confirmed manual dispatch on the default branch of one
# public portfolio repository may continue.
yq -r '.jobs.recover.steps[1].run' "$workflow" >"$work/admit.sh"
admit() {
  local confirmation="$2" private="$4"
  [[ "$confirmation" != EMPTY ]] || confirmation=""
  [[ "$private" != EMPTY ]] || private=""
  rm -f "$work/output"
  EVENT_NAME="$1" CONFIRMATION="$confirmation" REPOSITORY="$3" PRIVATE="$private" CURRENT_REF="$5" DEFAULT_BRANCH=main \
    GITHUB_OUTPUT="$work/output" bash -euo pipefail "$work/admit.sh" >/dev/null 2>&1
}
admit workflow_dispatch true devantler-tech/platform false refs/heads/main || fail 'confirmed recovery refused'
[[ "$(cat "$work/output")" == name=platform ]] || fail 'repository scope was not derived exactly'
while read -r event confirmation repository private ref; do
  if admit "$event" "$confirmation" "$repository" "$private" "$ref"; then
    fail "unsafe invocation admitted: $event $confirmation $repository $private $ref"
  fi
  [[ ! -e "$work/output" ]] || fail 'rejected admission emitted a repository scope'
done <<'CASES'
workflow_dispatch false devantler-tech/platform false refs/heads/main
workflow_dispatch EMPTY devantler-tech/platform false refs/heads/main
push true devantler-tech/platform false refs/heads/main
schedule true devantler-tech/platform false refs/heads/main
workflow_dispatch true devantler-tech/platform true refs/heads/main
workflow_dispatch true devantler-tech/platform EMPTY refs/heads/main
workflow_dispatch true devantler-tech/platform false refs/heads/feature
workflow_dispatch true devantler-tech/platform false refs/tags/v1.2.3
workflow_dispatch true other/platform false refs/heads/main
workflow_dispatch true devantler-tech/ksail false refs/heads/main
workflow_dispatch true devantler-tech/.. false refs/heads/main
workflow_dispatch true devantler-tech/../platform false refs/heads/main
CASES
# Run the shipped read admission against a stand-in inspector: only a missing release asks for
# the writer token; a present release is a verified no-op; anything else fails.
yq -r '.jobs.recover.steps[6].run' "$workflow" >"$work/read-admission.sh"
read_admission() {
  mkdir -p "$work/admission/.devantler-tech-recovery"
  printf '#!/usr/bin/env bash\nprintf %%s %q\nexit %s\n' "$1" "$2" >"$work/admission/.devantler-tech-recovery/inspect-release-recovery"
  chmod +x "$work/admission/.devantler-tech-recovery/inspect-release-recovery"
  rm -f "$work/output"
  (
    cd "$work/admission"
    RUNNER_TEMP="$work/runner" GITHUB_OUTPUT="$work/output" REPOSITORY=devantler-tech/platform TAG=v1.2.3 \
      SOURCE_SHA=a RUN_ID=1 RUN_ATTEMPT=1 PRODUCER_REF=b bash -euo pipefail "$work/read-admission.sh" >/dev/null 2>&1
  )
}
mkdir -p "$work/runner"
read_admission '{"diagnosis":"missing-github-release"}' 0 || fail 'missing release was not admitted'
grep -qx 'missing=true' "$work/output" || fail 'missing release did not request publication'
read_admission '{"diagnosis":"github-release-present"}' 0 || fail 'present release was not a no-op'
grep -qx 'missing=false' "$work/output" || fail 'present release requested a writer token'
for refused in '{"diagnosis":"other"}:0' '{}:0' 'not json:0' '{"diagnosis":"missing-github-release"}:1'; do
  if read_admission "${refused%:*}" "${refused##*:}"; then fail "unsafe read admission accepted: $refused"; fi
  if [[ -e "$work/output" ]] && grep -q '^missing=true$' "$work/output"; then fail "refused admission requested publication: $refused"; fi
done
# Execute the shipped source retrieval with only the HTTP transport replaced. Ambient
# credentials must not cross the anonymous immutable-source boundary.
yq -r '.jobs.recover.steps[2].run' "$workflow" >"$work/download.sh"
yq -r '.jobs["offline-test"].steps[1].run' "$workflow" >"$work/offline-download.sh"
cmp -s "$work/download.sh" "$work/offline-download.sh" || fail 'offline and production source retrieval differ'
[[ "$(yq -r '.jobs.recover.steps[4].run' "$workflow")" == "$(yq -r '.jobs["offline-test"].steps[3].run' "$workflow")" ]] ||
  fail 'offline and production builds differ'
source_dir="$work/archive/catalogue/.github/scripts/inspect-release-recovery"
mkdir -p "$work/bin" "$source_dir/cmd/publish-release-recovery"
printf 'module fixture\n\ngo 1.23.0\n' >"$source_dir/go.mod"
printf 'package main\nfunc main() {}\n' >"$source_dir/main.go"
printf 'package main\nfunc main() {}\n' >"$source_dir/cmd/publish-release-recovery/main.go"
tar -czf "$work/payload.tar.gz" -C "$work/archive" catalogue
cat >"$work/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
fixture="$(cd "$(dirname "$0")/.." && pwd -P)"
printf 'request\n' >> "$fixture/transport-used"
[[ -z "${GH_TOKEN-}${GITHUB_TOKEN-}${CURL_HOME-}${HTTPS_PROXY-}" ]]
expected=(--disable --fail --silent --show-error --location
  --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 120 --retry 3
  --output "$fixture/runner/recovery.tar.gz"
  'https://codeload.github.com/devantler-tech/.github/tar.gz/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')
[[ "$#" == "${#expected[@]}" ]]
index=0
for arg in "$@"; do [[ "$arg" == "${expected[index]}" ]]; index=$((index+1)); done
cp "$fixture/payload.tar.gz" "$fixture/runner/recovery.tar.gz"
CURL
chmod +x "$work/bin/curl"
download() {
  rm -rf "$work/download" && mkdir "$work/download"
  (
    cd "$work/download"
    PATH="$work/bin:$PATH" RUNNER_TEMP="$work/runner" CATALOGUE_REPOSITORY="$1" CATALOGUE_SHA="$2" \
      GH_TOKEN=must-not-forward CURL_HOME=must-not-forward HTTPS_PROXY=must-not-forward \
      bash -euo pipefail "$work/download.sh" >/dev/null 2>&1
  )
}
download devantler-tech/.github aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa || fail 'immutable source was not retrieved'
[[ -f "$work/download/.devantler-tech-recovery/.github/scripts/inspect-release-recovery/cmd/publish-release-recovery/main.go" ]] ||
  fail 'publisher source was not retrieved'
rm "$work/transport-used"
for invalid in 'devantler-tech/.github main' 'other/catalogue aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; do
  # shellcheck disable=SC2086 # Split the fixture pair into repository and revision.
  if download $invalid; then fail "untrusted recovery source accepted: $invalid"; fi
  [[ ! -e "$work/transport-used" ]] || fail 'invalid catalogue source reached the network'
done
rm "$source_dir/cmd/publish-release-recovery/main.go"
tar -czf "$work/payload.tar.gz" -C "$work/archive" catalogue
if download devantler-tech/.github aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; then fail 'incomplete recovery source accepted'; fi
[[ "${3:-}" != --guard-only ]] || exit 0
# The required native offline fixture builds the pinned publisher and executes its credential refusal.
printf "%s\n" "PASS: confirmed Platform recovery, immutable source, read admission and required offline coverage"
