#!/usr/bin/env bash
#
# Fail when the unsigned probe image could become signed, or depended on.
#
# .github/workflows/publish-unsigned-probe-image.yaml publishes
# ghcr.io/devantler-tech/unsigned-probe-throwaway, the negative control for
# probe-image-signature-enforcement.yaml (#3336). The probe only proves
# something while that image carries NO signature. A single added signing step
# would make the probe's refusal check meaningless without any check going red,
# so this guard makes the "unsigned" property something CI enforces.
#
# WHAT IT CHECKS, AND WHY EACH IS A WHITELIST.
# Every check below states what IS allowed rather than listing what is not. A
# list of forbidden signing tools misses the next one; a list of allowed
# actions, secrets and triggers does not.
#   1. The publisher's only trigger is `workflow_dispatch`.
#   2. No job or workflow-level permission grants `id-token`, permissions are
#      always written as a map, and the only permissions granted anywhere are
#      `packages` (read or write) and `contents: read`. Keyless signing needs
#      the OIDC token, so without it no step can sign, whatever it runs.
#   3. The only actions used are runner hardening and checkout. Any signing,
#      attestation or build action has to arrive through `uses:`.
#   4. The only secret referenced is GITHUB_TOKEN. Key-based signing needs a
#      key, and a key can only arrive as a secret.
#   5. No executable line names a signing or attestation tool, and provenance
#      and SBOM output stay disabled. This catches a tool installed and run by
#      hand, which checks 3 and 4 cannot see.
#   6. The workflow publishes exactly the throwaway image name.
#   7. No tracked file outside a fixed set names the image, so no manifest,
#      workload or other workflow can come to reference it.
#   8. The probe workflow's header still names the image, so the documented
#      ref and the published ref cannot drift apart.
#   9. No workflow in the repository runs on a package-publish event, so
#      publishing the image cannot start anything that might sign it.
#  10. A step after the push runs scripts/verify-image-unsigned.sh on the pushed
#      digest as its first command, authenticated with GITHUB_TOKEN, and can be
#      neither skipped nor run past. The script's own suite pins what it
#      checks; this check pins that it actually runs.
#  11. The first step refuses to run unless dispatched from refs/heads/main,
#      before any build, login or push, and nothing can skip or run past it.
#
# Nothing here can stop someone signing the image by hand from outside this
# repository. The publisher checks the published digest is unsigned for that
# reason, and the probe checks it again just before its node pull.
#
# Exit status: 0 clean, 1 a violation, 2 cannot check (a missing file, a
# missing tool, or a file yq cannot parse). 2 is never a pass.

set -euo pipefail

repo_root="${1:-.}"
cd "$repo_root"

readonly image='ghcr.io/devantler-tech/unsigned-probe-throwaway'
readonly image_name='unsigned-probe-throwaway'
readonly publisher='.github/workflows/publish-unsigned-probe-image.yaml'
readonly probe='.github/workflows/probe-image-signature-enforcement.yaml'
readonly verifier='scripts/verify-image-unsigned.sh'
readonly allowed_refs=(
  "$publisher"
  "$probe"
  'scripts/guard-unsigned-probe-image-publisher.sh'
  'scripts/tests/test-guard-unsigned-probe-image-publisher.sh'
)

status=0

violation() {
  printf '::error file=%s::%s\n' "$1" "$2"
  status=1
}

cannot_check() {
  printf '::error::cannot check: %s\n' "$1"
  exit 2
}

command -v yq >/dev/null 2>&1 || cannot_check 'yq is not installed'
command -v git >/dev/null 2>&1 || cannot_check 'git is not installed'
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || cannot_check "$PWD is not a git work tree"
[[ -f "$publisher" ]] || cannot_check "$publisher does not exist"
[[ -f "$probe" ]] || cannot_check "$probe does not exist"
yq -e '.' "$publisher" >/dev/null 2>&1 || cannot_check "yq cannot parse $publisher"

# `on` may be a string, a list or a map. Read all three shapes. The outer
# parentheses are load-bearing: `,` binds tighter than `|` in yq, so without
# them the list and map branches run against the whole document and report its
# top-level keys as triggers.
triggers_of() {
  yq -r '.on | ((select(tag == "!!str")), (select(tag == "!!seq") | .[]), (select(tag == "!!map") | keys | .[]))' "$1"
}

# 1. Triggers.
triggers="$(triggers_of "$publisher")" || cannot_check "could not read the triggers of $publisher"
if [[ "$triggers" != 'workflow_dispatch' ]]; then
  violation "$publisher" "the only allowed trigger is workflow_dispatch, found: $(printf '%s' "$triggers" | tr '\n' ' ')"
fi

# 2. Permissions.
id_token="$(yq -r '[.. | select(tag == "!!map" and has("id-token"))] | length' "$publisher")"
if [[ "$id_token" != '0' ]]; then
  violation "$publisher" 'grants an id-token permission; without it keyless signing is impossible, so it must never be granted here'
fi
scalar_permissions="$(yq -r '[.. | select(tag == "!!map" and has("permissions")) | .permissions | select(tag != "!!map")] | length' "$publisher")"
if [[ "$scalar_permissions" != '0' ]]; then
  violation "$publisher" 'sets permissions as a scalar (such as write-all), which grants id-token implicitly; write permissions as a map'
fi
# Every granted permission is read as a plain `key: value` line and judged in
# bash, one fact at a time (see check 10 for why nothing is combined in yq).
while IFS= read -r grant; do
  [[ -n "$grant" ]] || continue
  case "$grant" in
    'packages: write' | 'packages: read' | 'contents: read') ;;
    *) violation "$publisher" "grants '$grant'; the only allowed permissions are packages: read or write, and contents: read" ;;
  esac
done < <(yq -r '.. | select(tag == "!!map" and has("permissions")) | .permissions | select(tag == "!!map") | to_entries | .[] | .key + ": " + (.value | tostring)' "$publisher")

# 3. Actions.
while IFS= read -r uses; do
  [[ -n "$uses" ]] || continue
  case "$uses" in
    step-security/harden-runner@* | actions/checkout@*) ;;
    *) violation "$publisher" "uses '$uses'; the only allowed actions are step-security/harden-runner and actions/checkout" ;;
  esac
done < <(yq -r '.. | select(tag == "!!map" and has("uses")) | .uses' "$publisher")

# Executable text: every non-comment line. Comments are dropped first, so the
# header can explain what is forbidden without tripping the checks below.
code="$(grep -vE '^[[:space:]]*#' "$publisher" || true)"

# 4. Secrets.
while IFS= read -r secret; do
  [[ -n "$secret" ]] || continue
  if [[ "$secret" != 'secrets.GITHUB_TOKEN' ]]; then
    violation "$publisher" "references $secret; the only allowed secret is GITHUB_TOKEN"
  fi
done < <(printf '%s\n' "$code" | grep -oE 'secrets\.[A-Za-z0-9_]+' | sort -u || true)
if printf '%s\n' "$code" | grep -qE 'secrets\[|toJSON\(secrets'; then
  violation "$publisher" 'reads secrets indirectly; name GITHUB_TOKEN explicitly'
fi

# 5. Signing and attestation tooling.
signing="$(printf '%s\n' "$code" | grep -inE 'cosign|sigstore|notation|notary|attest|in-toto|slsa' || true)"
if [[ -n "$signing" ]]; then
  violation "$publisher" "names a signing or attestation tool: $(printf '%s' "$signing" | head -n 1)"
fi
# Match the build flag or action input, not the bare word.
metadata="$(printf '%s\n' "$code" | sed -E 's/--(provenance|sbom)=false//g' | grep -inE -- '--(provenance|sbom)|(^|[[:space:]])(provenance|sbom):' || true)"
if [[ -n "$metadata" ]]; then
  violation "$publisher" "enables provenance or SBOM output: $(printf '%s' "$metadata" | head -n 1)"
fi

# 6. Image name.
jobs="$(yq -r '.jobs | length' "$publisher")"
published="$(yq -r '[.jobs[].env.IMAGE] | .[]' "$publisher")"
if [[ "$jobs" != '1' || "$published" != "$image" ]]; then
  violation "$publisher" "must have exactly one job whose env.IMAGE is $image (found $jobs job(s), IMAGE='$(printf '%s' "$published" | tr '\n' ' ')')"
fi

# 7. References.
referencing="$(git grep -l -F "$image_name" -- . || true)"
if [[ -z "$referencing" ]]; then
  cannot_check "no tracked file names $image_name, not even the publisher; the reference sweep examined nothing"
fi
while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  allowed=0
  for candidate in "${allowed_refs[@]}"; do
    [[ "$path" == "$candidate" ]] && allowed=1
  done
  if ((allowed == 0)); then
    violation "$path" "references $image_name; the throwaway image must stay referenced by no manifest, workload or other workflow"
  fi
done <<<"$referencing"

# 8. Probe documentation.
if ! grep -qF "$image" "$probe"; then
  violation "$probe" "no longer names $image; its header must document the negative-control ref"
fi

# 9. Package-publish triggers anywhere.
workflows="$(git ls-files -- '.github/workflows/*.yaml' '.github/workflows/*.yml')"
[[ -n "$workflows" ]] || cannot_check 'found no tracked workflows'
while IFS= read -r workflow; do
  [[ -n "$workflow" ]] || continue
  workflow_triggers="$(triggers_of "$workflow" 2>/dev/null)" || cannot_check "yq cannot read the triggers of $workflow"
  if printf '%s\n' "$workflow_triggers" | grep -qxE 'registry_package|package'; then
    violation "$workflow" 'runs on a package-publish event, so publishing the unsigned probe image would start it'
  fi
done <<<"$workflows"

# 10. Post-push verification.
#
# The verification is behaviour, so it lives in a tested script rather than in
# workflow text a grep could only look at. What this check pins is that the
# script actually RUNS on the pushed digest. Matching fragments of the step's
# text is not enough: a step whose checks are commented out still contains the
# fragments. So the step must have exactly this shape, judged line by line after
# trimming, with blank lines ignored:
#
#     set -euo pipefail
#     ./scripts/verify-image-unsigned.sh --image "${IMAGE}@${DIGEST}"
#
# as its first two lines. A commented-out call, a call followed by `|| true`, or
# a call wrapped in a conditional is a different line and does not count.
#
# yq reads one fact per call and bash decides: a single yq `select` combining
# conditions with `and` matched steps it should not.
readonly verify_call="./scripts/verify-image-unsigned.sh --image \"\${IMAGE}@\${DIGEST}\""

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

# first_lines <text> — prints the first two non-blank lines, trimmed.
first_lines() {
  local line trimmed count=0
  while IFS= read -r line; do
    trimmed="$(trim "$line")"
    [[ -n "$trimmed" ]] || continue
    printf '%s\n' "$trimmed"
    count=$((count + 1))
    ((count < 2)) || break
  done <<<"$1"
}

if ! git ls-files --error-unmatch -- "$verifier" >/dev/null 2>&1; then
  violation "$verifier" "$verifier is missing or untracked; the publisher's verification step calls it"
fi

step_count="$(yq -r '.jobs[].steps | length' "$publisher")"
[[ "$step_count" =~ ^[0-9]+$ ]] || cannot_check "could not count the steps of $publisher (got '$step_count')"
push_index=''
verify_index=''
for ((i = 0; i < step_count; i++)); do
  step_id="$(yq -r ".jobs[].steps[$i].id // \"\"" "$publisher")"
  step_run="$(yq -r ".jobs[].steps[$i].run // \"\"" "$publisher")"
  if [[ -z "$push_index" && "$step_id" == 'push' ]]; then
    push_index="$i"
  fi
  if [[ -z "$verify_index" ]] && [[ "$(first_lines "$step_run")" == "set -euo pipefail"$'\n'"$verify_call" ]]; then
    verify_index="$i"
  fi
done
if [[ -z "$verify_index" ]]; then
  violation "$publisher" "must verify the pushed digest is unsigned: no step runs $verify_call as its first command after set -euo pipefail"
else
  if [[ -z "$push_index" ]] || ((verify_index <= push_index)); then
    violation "$publisher" 'the unsigned verification step must run after the step with id push'
  fi
  for key in continue-on-error if; do
    has_key="$(yq -r ".jobs[].steps[$verify_index] | has(\"$key\")" "$publisher")"
    [[ "$has_key" == 'true' || "$has_key" == 'false' ]] || cannot_check "could not read the verification step of $publisher (got '$has_key')"
    if [[ "$has_key" != 'false' ]]; then
      violation "$publisher" "the unsigned verification step must not set $key, so it can be neither skipped nor run past"
    fi
  done
  verify_password="$(yq -r ".jobs[].steps[$verify_index].env.REGISTRY_PASSWORD // \"\"" "$publisher")"
  verify_username="$(yq -r ".jobs[].steps[$verify_index].env.REGISTRY_USERNAME // \"\"" "$publisher")"
  verify_digest="$(yq -r ".jobs[].steps[$verify_index].env.DIGEST // \"\"" "$publisher")"
  if [[ "$verify_password" != *'secrets.GITHUB_TOKEN'* || -z "$verify_username" ]]; then
    violation "$publisher" 'the unsigned verification step must authenticate with GITHUB_TOKEN (env REGISTRY_USERNAME and REGISTRY_PASSWORD)'
  fi
  if [[ "$verify_digest" != *'steps.push.outputs.digest'* ]]; then
    violation "$publisher" 'the unsigned verification step must check the digest the push step reported (env DIGEST from steps.push.outputs.digest)'
  fi
fi

# 11. Reviewed ref only. A dispatch can target any branch or tag, so the very
# first step must refuse anything but main before any build, login or push. It
# must be a plain script step that nothing can skip (`if`) or run past
# (`continue-on-error`), and it must hold the exact comparison, so flipping it
# to `==` is caught too.
ref_check="[[ \"\${GITHUB_REF}\" != 'refs/heads/main' ]]"
first_has_run="$(yq -r '.jobs[].steps[0] | has("run")' "$publisher")"
first_has_uses="$(yq -r '.jobs[].steps[0] | has("uses")' "$publisher")"
first_has_if="$(yq -r '.jobs[].steps[0] | has("if")' "$publisher")"
first_has_coe="$(yq -r '.jobs[].steps[0] | has("continue-on-error")' "$publisher")"
first_run="$(yq -r '.jobs[].steps[0].run // ""' "$publisher")"
for fact in "$first_has_run" "$first_has_uses" "$first_has_if" "$first_has_coe"; do
  [[ "$fact" == 'true' || "$fact" == 'false' ]] || cannot_check "could not read the first step of $publisher (got '$fact')"
done
if [[ "$first_has_run" != 'true' || "$first_has_uses" != 'false' || "$first_has_if" != 'false' || "$first_has_coe" != 'false' ]] ||
  [[ "$first_run" != *"$ref_check"* || "$first_run" != *'exit 1'* ]]; then
  violation "$publisher" "must refuse to run from any ref other than refs/heads/main as its first step, before any build, login or push (a plain run step, with no uses, if or continue-on-error, containing: $ref_check ... exit 1)"
fi

if ((status == 0)); then
  printf 'unsigned probe image guard OK: %s stays unsigned and unreferenced\n' "$image"
fi

exit "$status"
