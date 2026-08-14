#!/usr/bin/env bash
# Decide whether this DR rebuild run still has work to do, or whether an
# equivalent rebuild already finished while this one was waiting its turn.
#
# Why this exists
# ---------------
# dr-rebuild.yaml holds two concurrency locks: `dr-rebuild` at workflow level
# (queue: single) to bound DR requests against each other, and `prod-deploy` at
# job level (queue: max) to serialize the rebuild against ordinary deploys.
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
# ...but equivalence also depends on `restore`. That dispatch input decides
# whether the Velero resource restore and the OpenBao raft-snapshot recovery run
# at all, so a restore=false rebuild is NOT equivalent to a restore=true one.
# Treating them as equal is a data-loss path: a first responder dispatches
# restore=false to get the platform up, a second dispatches restore=true while
# that is still running, the first succeeds, and the second is skipped as
# "already satisfied" — leaving prod up, EMPTY, and reporting success. So a
# superseder must have restored at least as much as this run asked for.
#
# The Actions API does not expose workflow_dispatch inputs on a run object, so
# the flag is published in the run name (see run-name in dr-rebuild.yaml) and
# read back from display_title. A run whose name does not carry the flag reads
# as "unknown" and supersedes nothing.
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

# restore_of_title recovers the published restore flag from a run's rendered
# name. A name that does not carry it — a run predating run-name, or a renamed
# workflow — reads "unknown", which supersedes nothing. The lookahead stops
# "restore=trueish" from reading as true.
#
# is_instant asserts the timestamp shape before it is compared. The comparison
# below is a lexicographic string compare, so any non-timestamp sorts as newer
# than a real instant ('"not-a-timestamp" > "2026-01-01T00:00:00Z"' is true in
# jq) and would skip a wanted rebuild. GitHub always returns a well-formed
# instant, so this is hardening rather than a live defect — but the whole point
# of this guard is that a wrong answer stays invisible until an incident.
# shellcheck disable=SC2016  # $t is a jq parameter; the shell must not expand it.
readonly jq_prelude='
  def restore_of_title($t):
    [ ($t // "") | match("restore=(true|false)(?![a-z])") ]
    | if length == 0 then "unknown" else .[0].captures[0].string end;
  def is_instant: test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$");
'

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
self="$(
  printf '%s' "${payload}" |
    jq -er --argjson id "${GITHUB_RUN_ID}" "${jq_prelude}"'
      first(.workflow_runs[]
        | select(.id == $id)
        | select(.created_at | is_instant)
        | "\(.created_at)\t\(restore_of_title(.display_title))")
    ' 2>/dev/null
)" || fail "This run (${GITHUB_RUN_ID}) was not found in the first page of ${workflow_file} runs, or carries an unusable creation timestamp, so supersession could not be decided.
Nothing destructive has happened. Re-run this job; if it keeps failing, check
the run list by hand before re-dispatching."

created_at="${self%%$'\t'*}"
self_restore="${self##*$'\t'}"

# A superseding run is any OTHER run of this workflow that reached a successful
# terminal state after this run was created AND restored at least as much.
# status/conclusion are checked explicitly so an in-progress or failed run never
# counts; an unknown restore flag never supersedes.
superseding="$(
  printf '%s' "${payload}" |
    jq -er --argjson id "${GITHUB_RUN_ID}" \
      --arg created "${created_at}" --arg self_restore "${self_restore}" "${jq_prelude}"'
      [ .workflow_runs[]
        | select(.id != $id
                 and .status == "completed"
                 and .conclusion == "success"
                 and (.updated_at | is_instant)
                 and .updated_at > $created)
        | . + {restore: restore_of_title(.display_title)}
        # restore=true satisfies any request; restore=false satisfies only a
        # request that did not ask for data back. unknown satisfies nothing.
        | select(.restore == "true" or (.restore == "false" and $self_restore == "false"))
      ]
      | sort_by(.updated_at)
      | last
      | if . == null then "" else "\(.id)\t\(.updated_at)\t\(.html_url)\t\(.restore)" end
    ' 2>/dev/null
)" || fail "Could not evaluate the run list for ${workflow_file}; refusing to rebuild on an undecided supersession check."

if [ -z "${superseding}" ]; then
  printf 'No DR rebuild has completed successfully since this run was created (%s, restore=%s).\n' \
    "${created_at}" "${self_restore}"
  printf 'Proceeding with the rebuild.\n'
  emit false
  exit 0
fi

IFS=$'\t' read -r superseding_id superseding_finished superseding_url superseding_restore \
  <<<"${superseding}"

printf '::notice::This rebuild was superseded and will be skipped.\n'
cat <<EOF
This run was created at ${created_at} with restore=${self_restore}, and DR
rebuild run ${superseding_id} completed successfully at ${superseding_finished}
with restore=${superseding_restore}:

  ${superseding_url}

A from-zero rebuild that finished after this run was requested, having restored
at least as much as it asked for, already satisfies it — so rebuilding again
would tear down and restore over the cluster that run has just recovered.
Skipping.

If production still needs rebuilding — because the recovery itself was
incomplete, or a NEW incident has happened since ${superseding_finished} —
dispatch DR - Rebuild Prod again. A dispatch made after that timestamp is not
superseded by it and will run.
EOF
emit true
