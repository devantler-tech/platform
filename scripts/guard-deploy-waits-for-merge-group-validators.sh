#!/usr/bin/env bash
#
# Fail when the merge-queue deploy can start before a validator that the required-checks gate
# enforces on the merge group has passed (#4148).
#
# WHY. `ci-required-checks` decides whether a merge group may land, but it runs after everything
# else. A validator that the gate requires and `deploy-prod` does not `need` runs in parallel with the
# deploy, so production can receive a revision that validator rejects. The group is then evicted,
# and prod carries content the gate refused until the heal restores it. `validate-matcher-efficacy`
# was in exactly that position: the one check that proves the cosign matcher accepts a real signer
# (#3005) did not order the deploy that depends on the matcher.
#
# WHICH JOBS ARE EXEMPT. Only a job that cannot run on the merge group has no result there for the
# deploy to wait on. That is read from the job's own `if:`, never from a hand-kept list: a job is
# pull-request-only when its guard is `github.event_name == 'pull_request'` on its own or as the
# first conjunct of a top-level `&&`, and it never mentions `merge_group`. Any other shape, including
# no `if:` at all, counts as running on the merge group. A misread therefore fails this check rather
# than letting an unordered validator through.
#
# Exit codes:
#   0  deploy-prod needs every merge-group validator ci-required-checks requires
#   1  at least one such validator is missing from deploy-prod.needs (each is named)
#   2  cannot check: bad usage, missing file, missing yq, unparseable YAML, or a missing job

set -uo pipefail

readonly gate_job='ci-required-checks'
readonly deploy_job='deploy-prod'

die() {
  printf 'guard-deploy-waits-for-merge-group-validators: %s\n' "$*" >&2
  exit 2
}

[ "$#" -eq 1 ] || die "usage: $0 <ci.yaml>"
file="$1"
[ -f "$file" ] || die "workflow '$file' does not exist"
command -v yq >/dev/null 2>&1 || die "yq is required but not installed"

# Each read checks its own status: a parse failure must be exit 2, never an empty list that reads as
# "nothing required".
needs_of() { # <job>
  JOB="$1" yq -r '.jobs[strenv(JOB)].needs | ((select(tag == "!!str")), (select(tag == "!!seq") | .[]))' "$file" 2>&1
}
job_exists() { # <job>
  JOB="$1" yq -e '.jobs | has(strenv(JOB))' "$file" >/dev/null 2>&1
}

for job in "$gate_job" "$deploy_job"; do
  job_exists "$job" || die "'$file' has no '$job' job (or cannot be parsed)"
done

required="$(needs_of "$gate_job")" || die "cannot read $gate_job.needs from '$file': $required"
[ -n "$required" ] || die "$gate_job declares no needs in '$file'"
deploy_needs="$(needs_of "$deploy_job")" || die "cannot read $deploy_job.needs from '$file': $deploy_needs"

pull_request_only() { # <if-expression>
  local expr="$1"
  case "$expr" in
    *merge_group*) return 1 ;;
  esac
  [ "$expr" = "github.event_name == 'pull_request'" ] && return 0
  case "$expr" in
    "github.event_name == 'pull_request' && "*) return 0 ;;
  esac
  return 1
}

missing=()
while IFS= read -r job; do
  [ -n "$job" ] || continue
  [ "$job" = "$deploy_job" ] && continue
  job_exists "$job" || die "$gate_job needs '$job', which is not a job in '$file'"
  if ! guard="$(JOB="$job" yq -r '.jobs[strenv(JOB)].if // ""' "$file" 2>&1)"; then
    die "cannot read $job.if from '$file': $guard"
  fi
  # Collapse whitespace so a folded multi-line `if:` compares like a one-line one.
  guard="$(printf '%s' "$guard" | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//')"
  if [ -n "$guard" ] && pull_request_only "$guard"; then
    continue
  fi
  if ! printf '%s\n' "$deploy_needs" | grep -qxF -- "$job"; then
    missing+=("$job")
  fi
done <<<"$required"

if [ "${#missing[@]}" -gt 0 ]; then
  for job in "${missing[@]}"; do
    printf 'guard-deploy-waits-for-merge-group-validators: %s requires %s on the merge group, but %s does not need it, so production can deploy a revision it rejects. Add it to %s.needs.\n' \
      "$gate_job" "$job" "$deploy_job" "$deploy_job" >&2
  done
  exit 1
fi

printf 'guard-deploy-waits-for-merge-group-validators: %s waits for every merge-group validator %s requires\n' "$deploy_job" "$gate_job"
