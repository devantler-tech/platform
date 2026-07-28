#!/usr/bin/env bash
# Decide whether this DR rebuild run still has work to do, or whether an
# equivalent rebuild already finished while this one was waiting its turn.
#
# Why this exists
# ---------------
# dr-rebuild.yaml holds two concurrency locks: `dr-rebuild` at workflow level
# (queue: single) to coalesce DR requests against each other, and `prod-deploy`
# at job level (queue: max) to serialize the rebuild against ordinary deploys.
#
# `queue: single` bounds the PENDING count to one; it does not discard a request
# because another is already running. So when a regular deploy holds
# `prod-deploy`, dispatch #1 takes the `dr-rebuild` slot and sits in progress
# waiting on the job lock, and dispatch #2 lands pending behind it rather than
# replacing it. Once #1 finishes, #2 runs a second from-zero teardown and
# restore over the cluster #1 has just recovered. That is the worst outcome on
# the most destructive path in the repository, and no value of `queue:` prevents
# it: coalescing DR-against-DR and queueing deploys-against-each-other are two
# different policies for one lock.
#
# So the supersession decision is made here, at execution, instead of by
# cancelling at dispatch. The worst case of checking late is a redundant no-op;
# the worst case of cancelling early is a teardown interrupted halfway.
#
# The test
# --------
# A dispatch means "make prod exist again from zero". If a from-zero rebuild
# COMPLETED SUCCESSFULLY after this run was created, that intent is already
# satisfied — a from-zero rebuild converges on the same end state regardless of
# when it started — so this run is redundant and reports superseded=true.
#
# The comparison is against this run's creation time, not its start time, which
# is what keeps a genuine second incident working: a rebuild that finished
# BEFORE the operator pressed the button again did not satisfy that later
# request, so the later run proceeds. A failed or cancelled earlier run never
# supersedes anything, so an ordinary retry-after-failure also proceeds.
#
# Failure is closed. The caller is a gate job that `rebuild` depends on, so any
# exit here other than a clean superseded=false decision leaves the destructive
# job unrun.

set -euo pipefail

# The workflow whose runs are compared. Kept explicit rather than derived so the
# accompanying test can assert this names the workflow that actually invokes the
# guard; a rename that misses one of the two fails CI instead of silently
# comparing an empty run list.
readonly workflow_file="dr-rebuild.yaml"

fail() {
  printf '::error::%s\n' "$1" >&2
  exit 1
}

emit() {
  # superseded is consumed by the gate job's output and the rebuild job's `if:`.
  printf 'superseded=%s\n' "$1" >>"${GITHUB_OUTPUT:-/dev/null}"
}

: "${GITHUB_RUN_ID:?GITHUB_RUN_ID must be set}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"

# Test seam: a file holding the workflow-runs payload, so the decision logic is
# exercised without network access or a token. Unset in CI and production.
runs_json="${DR_SUPERSESSION_RUNS_JSON:-}"

if [ -n "${runs_json}" ]; then
  [ -r "${runs_json}" ] || fail "DR_SUPERSESSION_RUNS_JSON is set but ${runs_json} is not readable."
  payload="$(cat "${runs_json}")"
else
  # Runs come back newest-first, so this run — which is currently executing — is
  # always on the first page, as is anything that completed since it was created.
  payload="$(
    gh api \
      "repos/${GITHUB_REPOSITORY}/actions/workflows/${workflow_file}/runs?per_page=100" 2>&1
  )" || fail "Could not list runs of ${workflow_file}: ${payload}
Nothing destructive has happened. Re-run this job once the API is reachable, or
confirm by hand that no other DR rebuild has completed since this one was
dispatched and then re-dispatch."
fi

# Fail closed on a payload we cannot read, rather than treating an unparseable
# response as "nothing superseded me" and rebuilding on top of a recovery.
created_at="$(
  printf '%s' "${payload}" |
    jq -er --argjson id "${GITHUB_RUN_ID}" \
      'first(.workflow_runs[] | select(.id == $id) | .created_at)' 2>/dev/null
)" || fail "This run (${GITHUB_RUN_ID}) was not found in the first page of ${workflow_file} runs, so supersession could not be decided.
Nothing destructive has happened. Re-run this job; if it keeps failing, check
the run list by hand before re-dispatching."

# A superseding run is any OTHER run of this workflow that reached a successful
# terminal state after this run was created. status/conclusion are checked
# explicitly so an in-progress or failed run never counts.
superseding="$(
  printf '%s' "${payload}" |
    jq -er --argjson id "${GITHUB_RUN_ID}" --arg created "${created_at}" '
      [ .workflow_runs[]
        | select(.id != $id
                 and .status == "completed"
                 and .conclusion == "success"
                 and .updated_at > $created)
      ]
      | sort_by(.updated_at)
      | last
      | if . == null then "" else "\(.id)\t\(.updated_at)\t\(.html_url)" end
    ' 2>/dev/null
)" || fail "Could not evaluate the run list for ${workflow_file}; refusing to rebuild on an undecided supersession check."

if [ -z "${superseding}" ]; then
  printf 'No DR rebuild has completed successfully since this run was created (%s).\n' "${created_at}"
  printf 'Proceeding with the rebuild.\n'
  emit false
  exit 0
fi

superseding_id="${superseding%%$'\t'*}"
superseding_rest="${superseding#*$'\t'}"
superseding_finished="${superseding_rest%%$'\t'*}"
superseding_url="${superseding_rest#*$'\t'}"

printf '::notice::This rebuild was superseded and will be skipped.\n'
cat <<EOF
This run was created at ${created_at}, and DR rebuild run ${superseding_id}
completed successfully at ${superseding_finished}:

  ${superseding_url}

A from-zero rebuild that finished after this run was requested already satisfies
it, so rebuilding again would tear down and restore over the cluster that run
has just recovered. Skipping.

If production still needs rebuilding — because the recovery itself was
incomplete, or a NEW incident has happened since ${superseding_finished} —
dispatch DR - Rebuild Prod again. A dispatch made after that timestamp is not
superseded by it and will run.
EOF
emit true
