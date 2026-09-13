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
#   2. No job or workflow-level permission grants `id-token`, and permissions
#      are always written as a map. Keyless signing needs the OIDC token, so
#      without it no step can sign, whatever it runs.
#   3. The only action used is the runner hardening step. Any signing,
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
#
# Nothing here can stop someone signing the image by hand from outside this
# repository. The publisher checks the published digest is unsigned for that
# reason.
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

# 3. Actions.
while IFS= read -r uses; do
  [[ -n "$uses" ]] || continue
  case "$uses" in
    step-security/harden-runner@*) ;;
    *) violation "$publisher" "uses '$uses'; the only allowed action is step-security/harden-runner" ;;
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
metadata="$(printf '%s\n' "$code" | sed -E 's/--(provenance|sbom)=false//g' | grep -inE 'provenance|sbom' || true)"
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

if ((status == 0)); then
  printf 'unsigned probe image guard OK: %s stays unsigned and unreferenced\n' "$image"
fi

exit "$status"
