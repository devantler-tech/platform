#!/usr/bin/env bash
# Guard the default-off release-recovery workflow: consent, scope, immutable source, the read-only
# job before the writer job, the built-in token only (no App key, no broker), and required CI
# coverage of consent/offline routing. Executes shipped steps.
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
  .w as $w | .c as $c | $w.jobs.inspect as $i | $i.steps as $s | $w.jobs.publish as $p | $p.steps as $t |
  $w.jobs["offline-test"] as $o |
  def ordinary: .if == null and .["continue-on-error"] == null;
  def recoveryContractRun: .run == "shellcheck scripts/generate-release-recovery-fixture.sh scripts/tests/test-release-recovery.sh\nbash scripts/tests/test-release-recovery.sh\n";
  def onlyIfMissing: .if == "steps.read-admission.outputs.missing == 'true'" and .["continue-on-error"] == null;
  def harden: (.uses | test("^step-security/harden-runner@[a-f0-9]{40}$")) and .with == {"egress-policy":"audit"};
  # setup-go defaults its token input to github.token; the toolchain step must override it empty.
  def setupGo: (.uses | test("^actions/setup-go@[a-f0-9]{40}$")) and (.with | keys) == ["cache","go-version","token"] and
    .with.cache == false and .with.token == "";
  def source: .env == {CATALOGUE_REPOSITORY:"devantler-tech/.github",CATALOGUE_SHA:"8e2dfbd3f9249b76b58234189ccbbae98d11b32b"} and
    (.run | contains("[[ \"$CATALOGUE_REPOSITORY\" == devantler-tech/.github && \"$CATALOGUE_SHA\" =~ ^[0-9a-f]{40}$ ]]")) and
    (.run | contains("env -i PATH=\"$PATH\" curl --disable")) and
    (.run | contains("https://codeload.github.com/$CATALOGUE_REPOSITORY/tar.gz/$CATALOGUE_SHA")) and
    (.run | contains("cmd/publish-release-recovery/main.go"));
  def build: .env == {GOWORK:"off",GOFLAGS:"",GOTOOLCHAIN:"local",CGO_ENABLED:"0"} and
    (.run | contains("build -trimpath -o \"$PWD/.devantler-tech-recovery/publish-release-recovery\" ./cmd/publish-release-recovery"));
  def incident: {REPOSITORY:"${{ github.repository }}",TAG:"${{ inputs.tag }}",SOURCE_SHA:"${{ inputs.source-sha }}",RUN_ID:"${{ inputs.run-id }}",RUN_ATTEMPT:"${{ inputs.run-attempt }}",PRODUCER_REF:"${{ inputs.producer-ref }}"};
  def builtInToken: "${{ github.token }}";
  ($w.on | keys) == ["workflow_dispatch"] and $w.permissions == {} and
  ($w.on.workflow_dispatch.inputs | has("enable-recovery") | not) and
  $w.on.workflow_dispatch.inputs["confirm-publication"].type == "boolean" and $w.on.workflow_dispatch.inputs["confirm-publication"].default == false and
  $w.on.workflow_dispatch.inputs["offline-test"].type == "boolean" and $w.on.workflow_dispatch.inputs["offline-test"].default == false and
  ($w.on.workflow_dispatch.inputs | has("repository") | not) and
  $w.on.workflow_dispatch.secrets == null and
  ($w.jobs | keys) == ["inspect","offline-test","publish"] and
  # The read job: admission, immutable source and build, then the only two token-bearing steps.
  $i.if == "inputs.confirm-publication && !inputs.offline-test" and $i.needs == null and $i["continue-on-error"] == null and
  $i.permissions == {contents:"read",actions:"read"} and
  $i.outputs == {missing:"${{ steps.read-admission.outputs.missing }}"} and
  ($s | length == 7) and ($s[0:6] | all(ordinary)) and ($s[6] | onlyIfMissing) and
  ($s[0] | harden) and
  $s[1].id == "admit" and
  $s[1].env == {EVENT_NAME:"${{ github.event_name }}",CONFIRMATION:"${{ inputs.confirm-publication }}",REPOSITORY:"${{ github.repository }}",PRIVATE:"${{ github.event.repository.private }}",CURRENT_REF:"${{ github.ref }}",DEFAULT_BRANCH:"${{ github.event.repository.default_branch }}"} and
  ($s[2] | source) and ($s[3] | setupGo) and ($s[4] | build) and
  $s[5].id == "read-admission" and $s[5].uses == null and $s[5].env == ({GH_TOKEN:builtInToken} + incident) and
  ($s[5].run | contains("\"./.devantler-tech-recovery/inspect-release-recovery\"")) and
  $s[6].id == "read-only-refusal" and $s[6].uses == null and $s[6].with == null and $s[6].shell == "bash" and
  $s[6].env == {GH_TOKEN:builtInToken,REPOSITORY:"${{ github.repository }}",DEFAULT_BRANCH:"${{ github.event.repository.default_branch }}"} and
  ($s[6].run | contains("env -i PATH=\"$PATH\" curl --disable")) and
  ($s[6].run | contains("https://api.github.com/repos/$REPOSITORY/releases/generate-notes")) and
  ([$s[] | select(.env.GH_TOKEN != null) | .id] == ["read-admission","read-only-refusal"]) and
  # The writer job: only after the read job proved a missing release and a refused read-only token.
  ($p.needs == "inspect" or $p.needs == ["inspect"]) and $p["continue-on-error"] == null and $p.outputs == null and
  $p.if == "inputs.confirm-publication && !inputs.offline-test && needs.inspect.outputs.missing == 'true'" and
  $p.permissions == {contents:"write",actions:"read"} and
  $p.concurrency == {group:"create-release-${{ github.repository }}-${{ github.ref }}","cancel-in-progress":false,queue:"max"} and
  ($t | length == 6 and all(ordinary)) and
  ($t[0] | harden) and $t[1] == $s[1] and $t[2] == $s[2] and $t[3] == $s[3] and $t[4] == $s[4] and
  $t[5].id == "publish" and $t[5].uses == null and $t[5].env == ({GH_TOKEN:builtInToken} + incident) and
  ($t[5].run | contains("\"./.devantler-tech-recovery/publish-release-recovery\"")) and
  ([$t[] | select(.env.GH_TOKEN != null) | .id] == ["publish"]) and
  # The built-in token reaches exactly those three steps; no stored key, App token or broker.
  ([$w | .. | strings | select(contains("github.token"))] | length == 3) and
  ([$w | .. | strings | select(test("secrets\\.|vars\\.|outputs\\.token|create-github-app-token|octo-sts"; "i"))] | length == 0) and
  $o.if == "inputs.offline-test" and $o.permissions == {} and
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
# The exact job predicates admitted above bind this closed routing table: inspect runs on a
# confirmed non-offline dispatch, publish only after inspect reports a missing release, and
# offline excludes both. Never execute an expression read from YAML as shell code.
routing_state() {
  local confirmation="$1" offline="$2" missing="$3" selected=none
  if [[ "$confirmation" == true && "$offline" == false ]]; then
    selected=inspect
    [[ "$missing" != true ]] || selected=inspect+publish
  fi
  if [[ "$offline" == true ]]; then
    [[ "$selected" == none ]] || fail 'offline and publication selected together'
    selected=offline
  fi
  printf '%s' "$selected"
}
while read -r confirmation offline missing expected; do
  [[ "$(routing_state "$confirmation" "$offline" "$missing")" == "$expected" ]] || fail 'confirmation/offline routing changed'
  printf 'PASS: confirmation=%s offline=%s missing=%s selects %s\n' "$confirmation" "$offline" "$missing" "$expected"
done <<'ROUTING'
false false false none
false false true none
false true false offline
false true true offline
true false false inspect
true false true inspect+publish
true true false offline
true true true offline
ROUTING
confirmation_default="$(yq -r '.on.workflow_dispatch.inputs["confirm-publication"].default' "$workflow")"
offline_default="$(yq -r '.on.workflow_dispatch.inputs["offline-test"].default' "$workflow")"
for missing in false true; do
  [[ "$(routing_state "$confirmation_default" "$offline_default" "$missing")" == none ]] || fail 'default inputs started a job'
done
# shellcheck disable=SC2016 # Keep GitHub expressions literal in refusal controls.
for mutation in \
  '.w.on.workflow_call={}' \
  '.w.on.workflow_dispatch.inputs["enable-recovery"]={type:"boolean",default:false}' \
  '.w.on.workflow_dispatch.inputs["confirm-publication"].default=true' \
  '.w.on.workflow_dispatch.inputs.repository={type:"string"}' \
  '.w.on.workflow_dispatch.inputs["offline-test"].default=true' \
  '.w.permissions={contents:"write"}' \
  '.w.jobs.extra={"runs-on":"ubuntu-latest",permissions:{contents:"write"},steps:[]}' \
  '.w.jobs.inspect.if="inputs.confirm-publication"' \
  '.w.jobs.inspect.if="!inputs.offline-test"' \
  '.w.jobs.inspect.permissions.contents="write"' \
  '.w.jobs.inspect.permissions["id-token"]="write"' \
  '.w.jobs.inspect["continue-on-error"]=true' \
  '.w.jobs.inspect.outputs.missing="true"' \
  '.w.jobs.inspect.steps[0].with.token="${{ github.token }}"' \
  '.w.jobs.inspect.steps[1].if="false"' \
  '.w.jobs.inspect.steps[1].env.CONFIRMATION="true"' \
  '.w.jobs.inspect.steps[2].env.CATALOGUE_SHA="main"' \
  '.w.jobs.inspect.steps[2].env.GH_TOKEN="${{ github.token }}"' \
  '.w.jobs.inspect.steps[3].with.cache=true' \
  '.w.jobs.inspect.steps[3].with |= del(.token)' \
  '.w.jobs.inspect.steps[3].with.token="${{ github.token }}"' \
  '.w.jobs.inspect.steps[4].env.GOFLAGS="-toolexec=untrusted"' \
  '.w.jobs.inspect.steps[4].env.GH_TOKEN="${{ github.token }}"' \
  '.w.jobs.inspect.steps |= [.[0],.[1],.[2],.[3],.[5],.[4],.[6]]' \
  '.w.jobs.inspect.steps |= [.[0],.[1],.[2],.[3],.[4],.[6],.[5]]' \
  '.w.jobs.inspect.steps[5].env.GH_TOKEN="${{ secrets.APP_PRIVATE_KEY }}"' \
  '.w.jobs.inspect.steps[6].if=null' \
  '.w.jobs.inspect.steps[6].env.DEFAULT_BRANCH="${{ inputs.tag }}"' \
  '.w.jobs.inspect.steps[6].if="false"' \
  '.w.jobs.inspect.steps[6]["continue-on-error"]=true' \
  '.w.jobs.inspect.steps |= map(select(.id != "read-only-refusal"))' \
  '.w.jobs.publish.needs=null' \
  '.w.jobs.publish.if="inputs.confirm-publication && !inputs.offline-test"' \
  '.w.jobs.publish.if="needs.inspect.outputs.missing == '\''true'\''"' \
  '.w.jobs.publish.if="always() && inputs.confirm-publication && !inputs.offline-test && needs.inspect.outputs.missing == '\''true'\''"' \
  '.w.jobs.publish["continue-on-error"]=true' \
  '.w.jobs.publish.permissions="write-all"' \
  '.w.jobs.publish.permissions.issues="write"' \
  '.w.jobs.publish.permissions["id-token"]="write"' \
  '.w.jobs.publish.concurrency=null' \
  '.w.jobs.publish.concurrency["cancel-in-progress"]=true' \
  '.w.jobs.publish.concurrency.group="recovery-${{ github.repository }}"' \
  '.w.jobs.publish.steps |= map(select(.id != "admit"))' \
  '.w.jobs.publish.steps[1].env.CONFIRMATION="true"' \
  '.w.jobs.publish.steps[2].env.CATALOGUE_SHA="main"' \
  '.w.jobs.publish.steps[3].with |= del(.token)' \
  '.w.jobs.publish.steps[4].env.GH_TOKEN="${{ github.token }}"' \
  '.w.jobs.publish.steps |= [.[0],.[1],.[2],.[3],.[5],.[4]]' \
  '.w.jobs.publish.steps[5].if="false"' \
  '.w.jobs.publish.steps[5]["continue-on-error"]=true' \
  '.w.jobs.publish.steps[5].env.REPOSITORY="${{ inputs.tag }}"' \
  '.w.jobs.publish.steps |= .[0:5] + [{id:"write-token",uses:"actions/create-github-app-token@f418cff399f7aef4c2f0ad47e25ca7a6abbdd6fc",with:{"client-id":"${{ vars.APP_CLIENT_ID }}","private-key":"${{ secrets.APP_PRIVATE_KEY }}"}}] + .[5:]' \
  '.w.jobs.publish.steps[5].env.GH_TOKEN="${{ steps.write-token.outputs.token }}"' \
  '.w.jobs.publish.steps |= .[0:5] + [{id:"broker",uses:"octo-sts/action@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",with:{scope:"devantler-tech/platform",identity:"recovery"}}] + .[5:]' \
  '.w.jobs["offline-test"].if="inputs.offline-test || inputs.confirm-publication"' \
  '.w.jobs["offline-test"].permissions={contents:"read"}' \
  '.w.jobs["offline-test"].steps[4].env={GH_TOKEN:"${{ secrets.APP_PRIVATE_KEY }}"}' \
  '.w.jobs["offline-test"].steps[4].env={GH_TOKEN:"${{ github.token }}"}' \
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
# Run the shipped admission block (both jobs carry it byte for byte): only a confirmed manual
# dispatch on the default branch of one public portfolio repository may continue.
yq -r '.jobs.inspect.steps[1].run' "$workflow" >"$work/admit.sh"
admit() {
  local confirmation="$2" private="$4"
  [[ "$confirmation" != EMPTY ]] || confirmation=""
  [[ "$private" != EMPTY ]] || private=""
  rm -f "$work/output"
  EVENT_NAME="$1" CONFIRMATION="$confirmation" REPOSITORY="$3" PRIVATE="$private" CURRENT_REF="$5" DEFAULT_BRANCH=main \
    GITHUB_OUTPUT="$work/output" bash -euo pipefail "$work/admit.sh" >/dev/null 2>&1
}
admit workflow_dispatch true devantler-tech/platform false refs/heads/main || fail 'confirmed recovery refused'
[[ ! -e "$work/output" ]] || fail 'admission emitted an output'
while read -r event confirmation repository private ref; do
  if admit "$event" "$confirmation" "$repository" "$private" "$ref"; then
    fail "unsafe invocation admitted: $event $confirmation $repository $private $ref"
  fi
  [[ ! -e "$work/output" ]] || fail 'rejected admission emitted an output'
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
# Run the shipped read admission against a stand-in inspector: only a missing release starts
# the publish job; a present release is a verified no-op; anything else fails.
yq -r '.jobs.inspect.steps[5].run' "$workflow" >"$work/read-admission.sh"
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
grep -qx 'missing=false' "$work/output" || fail 'present release requested publication'
for refused in '{"diagnosis":"other"}:0' '{}:0' 'not json:0' '{"diagnosis":"missing-github-release"}:1'; do
  if read_admission "${refused%:*}" "${refused##*:}"; then fail "unsafe read admission accepted: $refused"; fi
  if [[ -e "$work/output" ]] && grep -q '^missing=true$' "$work/output"; then fail "refused admission requested publication: $refused"; fi
done
# Execute the shipped source retrieval with only the HTTP transport replaced. Ambient
# credentials must not cross the anonymous immutable-source boundary.
yq -r '.jobs.inspect.steps[2].run' "$workflow" >"$work/download.sh"
yq -r '.jobs["offline-test"].steps[1].run' "$workflow" >"$work/offline-download.sh"
cmp -s "$work/download.sh" "$work/offline-download.sh" || fail 'offline and production source retrieval differ'
[[ "$(yq -r '.jobs.inspect.steps[4].run' "$workflow")" == "$(yq -r '.jobs["offline-test"].steps[3].run' "$workflow")" ]] ||
  fail 'offline and production builds differ'
# The writer job rebuilds from the same immutable source with the same commands.
[[ "$(yq -r '.jobs.publish.steps[2].run' "$workflow")" == "$(cat "$work/download.sh")" &&
  "$(yq -r '.jobs.publish.steps[4].run' "$workflow")" == "$(yq -r '.jobs.inspect.steps[4].run' "$workflow")" ]] ||
  fail 'the publish job does not rebuild the inspected source'
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
