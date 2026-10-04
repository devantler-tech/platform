# Diagnose a green PR blocked from the merge queue

🔴 **`BLOCKED` on a fully green head can mean an UNSATISFIED REQUIRED WORKFLOW — and nothing on the
PR says so.** ⚠️ **Rule out the ordinary blockers first**, or you will move the head without touching
the actual cause: `BLOCKED` on a green head is also what an unresolved review conversation, a missing
required approval, or any other non-check branch rule looks like. The hygiene pentad already requires
green checks **and** zero unresolved threads, so check those before reaching for this procedure —
what follows is for a head that is green *and* thread-clean *and* still `BLOCKED`.

Read the **effective rules for `main`**, including inherited organization rules, before naming a
required workflow. Rulesets and the workflows they require can change independently of this file.
The read-only resolver retains each path, source repository, ref and ruleset identity:

```bash
required_json="$(bash scripts/required-merge-workflows.sh)" || exit 1
printf '%s\n' "$required_json" | jq .
# Select the workflow record being diagnosed from this live result, not from a saved name.
managed=<path-from-the-live-workflow-record>
# Refuse a path that the live effective rules do not require.
printf '%s\n' "$required_json" | jq -e --arg managed "$managed" \
  'any(.[]; .path == $managed)' >/dev/null || exit 1
```

Diagnose **every** required workflow before judging the PR ready; one satisfied workflow does not
satisfy the others. Preserve the selected record's source repository/ref as well as its path for the
provenance check below. An unreadable or malformed response is UNKNOWN; do not guess a workflow name.
The run API queries below identify a **path and run status**, not the current required source
repository/ref. A source rule can change while retaining the same path. If source provenance cannot
be bound through authenticated native evidence, keep requirement satisfaction UNKNOWN. Only an
authorized exact-head enqueue, after the other readiness gates pass, lets GitHub confirm its current
policy; do not enqueue as a read-only diagnostic probe.
The workflow requirement is evaluated against a run **for the current head**, and a workflow
cannot fire retroactively — so a PR whose head predates the requirement can never be enqueued, no
matter what is done to it.

Every visible signal says such a PR is fine: all check runs `success`/`skipped` including the
required `CI - Required Checks`, **0** approvals needed, `mergeStateStatus: BLOCKED` with nothing on
the head explaining it, and **`gh pr merge` exits 0, prints nothing, and does not queue it** (on this
repo it silently arms auto-merge instead). The only surface that names the cause is the enqueue
mutation:

```text
enqueuePullRequest(input:{pullRequestId:"<id>", expectedHeadOid:"<head>"}) →
  UNPROCESSABLE: Pull request Required workflow '<title from the effective rule>' is not satisfied
```

🔴 **`enqueuePullRequest` is a WRITE, not a probe — it only *reads* like a diagnostic when it
fails.** If the diagnosis is wrong, or the PR became eligible between your inspection and the call,
it **succeeds**: the PR enters the merge queue and starts the `merge_group` run whose `deploy-prod`
job **deploys to the real prod cluster**. So call it only when you actually intend and are
authorised to enqueue that PR, and always pass **`expectedHeadOid`** pinned to the head you
inspected, so a head that moved under you is refused instead of silently queueing a revision nobody
assessed. To *diagnose* without that risk, use the read-only run queries below and treat the
mutation as the confirmation step.

**Diagnose by RUN, never by check-run name.** The workflow's *jobs* appear as check runs under their
own names (`🏗️ Build`, `🧪 Test`, …), so grepping check-run names for the workflow title reports zero
on a head that is perfectly satisfied. Ask which workflows ran instead:

**Did it fire at all?** — the diagnostic question, and the one that separates "this head predates the
requirement" from "the workflow ran and went red". `0` here is the defect this section is about:

```sh
set -euo pipefail
runs_json="$(gh api --paginate --slurp \
  "repos/devantler-tech/platform/actions/runs?head_sha=<head>&per_page=100")" || {
  echo 'workflow run read failed — UNKNOWN' >&2; exit 2
}
jq --arg managed "$managed" '[.[].workflow_runs[]
  | select(.path == $managed)] | length' <<< "$runs_json"
```

**Is the requirement satisfied?** — a completed successful path-matching run is necessary evidence,
but the source/provenance and native-policy confirmation above still apply:

```sh
set -euo pipefail
runs_json="$(gh api --paginate --slurp \
  "repos/devantler-tech/platform/actions/runs?head_sha=<head>&per_page=100")" || {
  echo 'workflow run read failed — UNKNOWN' >&2; exit 2
}
jq --arg managed "$managed" '[.[].workflow_runs[]
  | select(.path == $managed
           and .status == "completed"
           and .conclusion == "success")] | length' <<< "$runs_json"
```

⚠️ **Both queries must paginate, and `--paginate` ALONE is not the fix.** `per_page=100` caps one
page, so an unpaginated count can miss a run and report the `0` this section teaches you to read as
"the head predates the requirement" — the false negative that sends the diagnosis down the wrong
path. But this endpoint returns an **object** (`{total_count, workflow_runs}`), so `--paginate` emits
one object *per page* and a `--jq` filter runs **per page**: measured on a real head with
`per_page=1`, the naive form printed `0 0 0 0 1 0 0` — seven separate numbers, not a total, which a
numeric shell test then mis-reads or errors on. `--slurp` wraps the pages into one array, which is
why the filter moves to a standalone `jq` over `.[].workflow_runs[]`. `gh api` rejects `--slurp`
together with `--jq` (`the --slurp option is not supported with --jq or --template`), so the pipe is
required as a separate processing stage rather than stylistic. The examples capture and check the
API response before passing it to `jq`, so a later-page failure cannot emit a usable partial count.
Run these Bash fragments in the same operator script as the selected `managed` record; they are
not POSIX shell. A pipeline variant must retain `pipefail` and check its status before using output.

⚠️ **Do not use the first query to answer the second.** A run that is queued, in progress, cancelled or
**failed** still appears in `workflow_runs`, so a `>= 1` from the path-only form will report a red or
still-running head as ready to enqueue. Reading them the other way round is just as misleading: a `0`
from the strict form does not distinguish a head that never fired the workflow from one whose run
failed, which are opposite problems with opposite fixes — refresh the head, or fix the build.

⚠️ **Both queries key on `.path`, which does not prove PROVENANCE.** A PR that itself adds or edits
the selected workflow path produces runs carrying that exact path from the PR's own
copy — so the count comes back positive while the org-injected required workflow is still
unsatisfied, and the PR stays un-enqueueable for the very reason the query just said was fine. On any
PR that touches that file, confirm the file is **absent from the PR's own tree** before trusting
either count (the same provenance collision `scripts/check-megalinter-version-drift.sh` documents and
checks for), and otherwise fail closed and read the rejection reason from the mutation instead.

🔴 **A `0` read moments after the head appeared is not evidence of anything yet — wait before you act
on it.** Run creation and indexing lag behind the push, so the count comes back `0` while the required
workflow is merely still being created, and that `0` is **indistinguishable** from the one this section
is about. What makes it expensive rather than merely wrong is the answer it selects: the fix below is a
**write** that moves the head, so an unwaited read buys an unnecessary branch update — and on this repo
auto-merge may already be armed, which sends a revision nobody diagnosed toward the production-deploying
merge queue. Give the current head the **same bounded poll and the same UNKNOWN-on-timeout treatment**
the post-update poll below uses. This applies to **any** head you have not already watched a run appear
on — not just one you moved yourself.

⚠️ **An exhausted bound is UNKNOWN here too — a `0` surviving the poll is NOT by itself the
"predates the requirement" case.** The two reads differ in what a timeout can mean, and that is the
whole reason this needs saying: after *you* moved the head a run is definitely coming, so waiting
always terminates; on a head you did **not** move, a run may genuinely never appear, so the poll
cannot distinguish "still indexing" from "never fired" by waiting longer. Waiting is what rules out
the first; it can never establish the second.

🔴 **Neither of the two obvious "independent evidence" shortcuts actually works — do not reach for
them.** They are the first things that come to mind here, and both fail in the same direction, blessing
an absence as real and spending the head-moving write on it:

| Tempting evidence | Why it does not establish `predates` |
| --- | --- |
| *Some other workflow run is already indexed at this head, so indexing has completed* | Workflows are created and indexed **independently** — the poll below says so in its own comment, and relies on it. Another workflow appearing first says nothing about the managed one, which may still be mid-creation. |
| *The head commit's date precedes the requirement's introduction* | The requirement fires on **PR events, not commit dates**. An older commit can become the head afterwards — a force-push back to it, or a PR opened later off a stale branch — and the run then fires normally. |

The only conclusive evidence is about the **managed workflow itself**: a run for it observed at this
head (whatever its conclusion), or a PR head-event record showing the requirement was already in force
when this head was pushed and no such run was created. Absent that, carry it forward as UNKNOWN rather
than spending the head-moving write on a guess.

**The fix is to move the head** — `PUT /repos/{owner}/{repo}/pulls/{n}/update-branch` (a merge of
`main`, never a force-push) makes the workflow fire. **Pin it to the head you inspected**, exactly as
the enqueue mutation above is pinned:

```sh
# Capture the base tip FIRST. It is what the new head must later be proved to contain, and
# reading it after the request races a `main` that advanced while the update was in flight —
# which returns a non-zero `behind_by` and misreports your own successful update as someone
# else's push.
#
# ABORT if that read fails. Check the exit status and require a full sha: an API, network or
# auth error leaves base_tip empty, and an unguarded script then still issues the PUT below —
# performing the WRITE while having lost the ability to prove what landed. The later compare
# would build an empty-base URL, fail, and report the update as someone else's push.
if ! base_tip="$(gh api repos/devantler-tech/platform/commits/main --jq .sha)"; then
  echo "base tip unreadable - not moving the head" >&2; exit 1
fi
case "${base_tip}" in
  *[!0-9a-f]*|"") echo "base tip malformed: '${base_tip}'" >&2; exit 1 ;;
esac
[ ${#base_tip} -eq 40 ] || { echo "base tip not a full sha: '${base_tip}'" >&2; exit 1; }

# PREFLIGHT THE NO-BASE CASE — this read is what makes the empty-commit fallback REACHABLE.
# `update-branch` merges the base INTO the head, so a head that already contains the current
# tip of `main` has nothing to merge: the call cannot move the head or fire the workflow.
# Without this read the sequence goes straight from the tip to the PUT, and that case
# degrades into either a refused request or an unchanged-head timeout — which the outcome
# table below defines as UNKNOWN and explicitly forbids reading as "no base". The fallback
# would then be documented but unreachable from this sequence. Establish it POSITIVELY here,
# before the write, exactly as the fallback paragraph below requires.
#
# `behind_by` from `compare/{base}...{head}` counts commits the HEAD is behind the BASE, so
# `0` means the head already contains `base_tip`. This is the same read, in the same
# direction, that the post-update compare uses to prove the base landed.
if ! behind_before="$(gh api \
    "repos/devantler-tech/platform/compare/${base_tip}...<head>" --jq .behind_by)"; then
  echo "preflight compare failed - cannot tell whether there is base to merge" >&2; exit 1
fi
case "${behind_before}" in
  ''|*[!0-9]*) echo "behind_by malformed: '${behind_before}'" >&2; exit 1 ;;
esac
if [ "${behind_before}" -eq 0 ]; then
  # Evidenced no-base case. Do NOT issue the PUT — it cannot help. Exit distinctly so the
  # caller takes the empty-commit fallback rather than treating this as a failed diagnosis.
  echo "head already contains main (behind_by=0) - take the empty-commit fallback" >&2
  exit 2
fi

# GUARD THE WRITE. `gh` returns nonzero on a failed request, and the failure that matters
# here is the `422` raised when `expected_head_sha` no longer matches — i.e. another actor
# moved the head first. Unguarded, the sequence falls straight through into the poll below
# and starts interpreting a head THIS REQUEST NEVER CREATED. The downstream checks do not
# save you: if that actor's commit is a direct child carrying `base_tip`, it passes the
# ancestry test AND the parent test, so an uninspected revision is accepted as GitHub's
# update — and on this repo auto-merge may already be armed. Abort and re-pin instead.
if ! gh api --method PUT "repos/devantler-tech/platform/pulls/<n>/update-branch" \
    -f expected_head_sha=<head>; then
  echo "update-branch refused (head moved, or the request failed) - re-pin and restart" >&2
  exit 1
fi
```

🔴 **Unpinned, this write moves whatever head is current, which is not necessarily the one you
diagnosed.** A contributor or bot push landing between the inspection and this call makes the update
advance an uninspected revision — and because auto-merge may already be armed on this repo, that
revision proceeds into the **production-deploying** merge queue once its checks pass. With the pin,
GitHub refuses the mismatch with `422` and the diagnosis simply starts again, which is the cheap
failure.

🔴 **`202` is ASYNCHRONOUS, and the head you must query afterwards is a NEW one.** The API schedules
the update as a background task, so `202 Updating pull request branch` means *accepted*, not *done* —
and when it completes, the PR's head has **changed**. A single immediate read races that task and
returns the head you just moved away from, which is the one answer guaranteed to mislead: the
workflow-run queries would then inspect the old commit, report the same `0`, and the fix would look
to have failed at the moment it worked. Re-read until the head actually moves, bounded. For a human
operator, this bounded poll block runs as a **single script invocation**, including its ancestry
and direct-parent checks; never unroll it into per-iteration tool calls. Agents follow the canonical
monorepo waiting rules: at most one native bounded watcher, no foreground remote wait, and one-shot
reads when no native watcher is available. The block preserves the complete verification algorithm:

```sh
inspected=<the head you pinned the update to>
# `base_tip` is already set — it was captured above, BEFORE the update was requested.
new_head=""
probe=""
for _ in $(seq 1 30); do
  if ! probe="$(gh pr view <n> --repo devantler-tech/platform \
      --json headRefOid --jq .headRefOid)"; then
    probe=""
    break
  fi
  if [ "${probe}" != "${inspected}" ]; then
    new_head="${probe}"
    break
  fi
  sleep 2
done
# A MOVED head is not a LANDED update — prove the base is actually in it.
if [ -n "${new_head}" ]; then
  behind="$(gh api \
    "repos/devantler-tech/platform/compare/${base_tip}...${new_head}" \
    --jq .behind_by)" || behind=""
  [ "${behind}" = "0" ] || new_head=""   # someone else's push: re-pin and restart
fi
# Containment is NOT identity. `behind_by == 0` only proves base_tip is an ANCESTOR of
# new_head, which stays true when another actor pushes ON TOP of GitHub's merge — so the
# check above would accept that actor's extra, uninspected commit as the update result.
# GitHub's update-branch merge has the head you pinned as a DIRECT parent; a commit pushed
# on top demotes it to a grandparent. Require the parent relationship, and fail closed on a
# failed read rather than accepting an unverified head.
if [ -n "${new_head}" ]; then
  if parents="$(gh api "repos/devantler-tech/platform/commits/${new_head}" \
      --jq '.parents[].sha')"; then
    printf '%s\n' "${parents}" | grep -qxF -- "${inspected}" || new_head=""
  else
    new_head=""                          # unverified: re-pin and restart
  fi
fi
```

**Four outcomes, and they are not interchangeable** — collapsing them is how a working fix gets
reported as a failure, and a failed read gets reported as a fix:

| result | what happened | what to do |
| --- | --- | --- |
| `new_head` non-empty | the update landed **and the base is provably in it** | run **both** workflow-run queries against `new_head` — but only after a run for `new_head` actually exists (see below) |
| head moved but `behind_by` non-zero | someone else's push moved it, not your update | **fail closed**: re-pin to that head and restart the diagnosis |
| loop exhausted, head unchanged | **unknown** — either there was no base to merge, or the accepted update is still pending or failed after acceptance | conclude nothing from the timeout alone; carry the PR forward and require independent evidence before the empty-commit fallback |
| `probe` empty | the read failed, so the update's outcome is unknown | conclude nothing — re-read once before acting on either branch |

🔴 **A timeout is NOT the no-base case, and treating it as one pushes a commit you did not need.**
`202` means the update was *accepted*; an unchanged head after the bound is equally consistent with a
task that is delayed or that failed after acceptance. Row 3 therefore proves nothing on its own —
inferring "there was no base to merge" from it sends an agent to push the fallback empty commit while
the original update is still in flight, landing two head moves for one problem. The no-base case is
real and documented below, but it is established by **evidence** — the head already contains the
current tip of `main`, i.e. `behind_by` against `main` is `0` **before** the request — never by a
loop having run out.

🔴 **A run for the new head does not exist the instant the head does — poll for it, or its absence
lies.** Workflow-run creation and indexing lag behind the head update, so a query issued immediately
after `new_head` appears can return `0` runs — which this guide defines as evidence that *the head
predates the requirement*. A successful refresh is then misdiagnosed as a no-op and answered with yet
another branch update or empty commit. Wait for a matching run on `new_head` under its own bound
before interpreting any absence, and only then read its status:

**Run this one with Bash specifically** — it needs `set -o pipefail`, which is not POSIX. Under a
`/bin/sh` that is `dash` (the Debian and Ubuntu default) that line aborts the subshell with an
illegal-option error, so **every** iteration reads as a failed query and the poll returns UNKNOWN even
where the managed run exists — the one failure mode this poll is written to avoid:

For a human operator, this second bounded poll block also runs as a **single script invocation**;
keep its pagination, pipeline failure handling and UNKNOWN-on-timeout logic together. For agents,
the canonical native-watcher and one-shot-read rules described above still apply.

```bash
# Poll for the MANAGED workflow's own run, not merely for any run at the head. Workflows are
# created and indexed independently, so another PR workflow appearing first makes a total
# count non-zero while the required run is still absent — and the path-filtered query would
# then read that absence as "the head predates the requirement".
#
# POLL TO A TERMINAL STATUS, never to mere existence. A run that is `queued` or `in_progress`
# is already indexed, so an existence test breaks out here immediately — and the strict
# "is the requirement satisfied?" query above then answers `0`, which is the SAME answer it
# gives for a run that FAILED or was cancelled. Breaking early therefore reports a perfectly
# healthy still-running workflow as a red build, and sends the diagnosis off to fix a build
# that is fine — or to spend another head-moving write on it. Wait for the run to complete,
# then branch on its conclusion.
# Keep `managed` bound to the selected live effective workflow record above.
managed_run=""
managed_conclusion=""
for _ in $(seq 1 30); do
  # A FAILED read is UNKNOWN, never a hit. Check the exit status: on a transient API,
  # network or auth error the substitution collapses to the empty string, which carries no
  # observation at all — so it must reach neither `success` nor `failed`. The exit-status
  # test is the first line of that defence and the catch-all arm below is the second; a
  # verdict must come from a read that actually succeeded, which is the fail-open this poll
  # exists to prevent.
  # PAGINATE, exactly as the two diagnostic queries above must: `per_page=100` caps one page,
  # so a head with enough runs (repeated reruns, a busy PR) can push the managed run onto a
  # later page and make this poll return `0` forever — a false UNKNOWN that never terminates.
  # `--paginate` alone is wrong here for the documented reason: this endpoint returns an
  # object, so a `--jq` filter would run per page and emit one number per page instead of a
  # total. `--slurp` wraps the pages, and it cannot be combined with `--jq`, hence the pipe.
  # `set -o pipefail` in the subshell is REQUIRED, not tidiness: a command substitution takes
  # the status of the LAST element of the pipeline, so without it a failed `gh api` is masked
  # by a `jq` that exits 0 on empty input — and the exit-status check below would be reading
  # jq's success while gh had failed.
  # `--arg` rather than string-interpolating `${managed}` into the filter: the path is data,
  # and a quoted jq program keeps the shell out of it.
  if state="$(set -o pipefail
      gh api --paginate --slurp \
        "repos/devantler-tech/platform/actions/runs?head_sha=${new_head}&per_page=100" \
      | jq -r --arg managed "${managed}" '
          [.[].workflow_runs[] | select(.path == $managed)]
          | if   length == 0                    then "absent"
            elif any(.status != "completed")    then "running"
            elif any(.conclusion == "success")  then "success"
            else                                     "failed"
            end')"; then
    # Require a RECOGNISED verdict. Anything else — an error string, an empty body, a status
    # this repo has not seen — is UNKNOWN, and must keep waiting rather than break.
    case "${state}" in
      success) managed_run=1; managed_conclusion=success; break ;;
      failed)  managed_run=1; managed_conclusion=failed;  break ;;
      absent|running) ;;            # not terminal yet: keep waiting
      *) ;;                         # unrecognised: UNKNOWN, keep waiting
    esac
  fi
  sleep 2
done
# An exhausted bound is UNKNOWN — NOT evidence that the head predates the requirement.
# That is also what keeps this poll from being circular: it never has to *conclude* absence,
# so a head which genuinely predates the requirement ends at UNKNOWN rather than hanging.
# Carry it forward like any other unknown; do not answer it with a second head move.
#
# `managed_conclusion` now ANSWERS the "is the requirement satisfied?" question directly, and
# it is the same test that query applies (`status == "completed"` and some run `success`), so
# read it rather than re-issuing the strict query — re-reading only reopens the window where a
# still-running run answers `0`. `success` = a green path-matching run, subject to the source
# and native-policy confirmation above. `failed` = a genuinely red
# build to fix, NOT a head to move again. Empty = UNKNOWN, per the bound above.
```

⚠️ **The provenance collision above applies to this poll too.** A PR whose own tree contains
the selected workflow path produces runs carrying that exact path from its own copy, so `managed_run`
can be satisfied by the PR's own workflow rather than the org-injected one. Confirm the file is absent
from the PR's tree before trusting this poll, exactly as when trusting either count.

🔴 **A CHANGED head is not evidence your update landed — check what is IN it.**
`expected_head_sha` guards the request-time head only; it is not a lock, so nothing stops a bot or
a sibling lane pushing between the `202` and the completion. That push satisfies
`probe != inspected` on its own, so a head-moved test reports a landed update while the base was
never merged — and the workflow queries then run against a commit that is still behind, producing
exactly the misleading `0` this section exists to prevent. This is not hypothetical: **this
repository's own megalinter bot pushes fix-up commits to PR branches**, which is precisely such a
push. Comparing `behind_by` against the base tip captured *before* the request is what
distinguishes the two, and the honest answer when it is non-zero is to start again from the
newly-pinned head.

Never run the workflow-run queries against the head you moved away from: that commit's answer is
frozen and keeps reporting the same `0`. An unattended run that exhausts the bound records the PR as
a carry-forward rather than extending the wait.

A conflicting (`mergeable_state: dirty`) PR cannot be updated this way and needs its conflict resolved
first.

⚠️ **`update-branch` only works when there is base to merge.** It updates the branch *with the latest
changes of the base*, so a head that already contains the current tip of `main` — the case when the
ruleset was enabled with no `main` commit after it — has nothing to merge, and the call cannot move
the head or fire the workflow. It is not an unconditional fix. **Establish this case positively
before acting on it** — `behind_by` against `main` is `0` for the head you inspected, read *before*
the request — never by inferring it from an exhausted polling bound, which is the unknown in row 3
above. Once it is evidenced, create a new head event some other way: push an empty commit
(`git commit --allow-empty -m "chore: refresh head for required workflow"`) on the PR branch, which
is the cheapest thing that makes the workflow fire. Never force-push a branch to achieve this.

Any locally created head-refresh commit must be **signed** and pass the canonical unsigned-push
guard before publication. Freshly rebind ownership, the head, checks and reviews before any branch
write; a different session's update supersedes the earlier diagnosis. Re-read the effective rules
if they changed during the diagnosis, and repeat this procedure for every required workflow.

**Never judge an enqueue by `gh pr merge`'s exit code.** Read the rejection reason from the
`enqueuePullRequest` mutation, and assert the effect with GraphQL `isInMergeQueue` +
`mergeQueueEntry{state,position}` — silence is not failure and exit 0 is not success.

⚠️ **`autoMergeRequest` answers a DIFFERENT question, so do not read it as the queue state.** It
stays `null` while a PR is genuinely **queued** (which is why `isInMergeQueue` is the queue test) —
but it is **populated** once auto-merge has been *armed*, which is exactly what `gh pr merge` does
silently on this repo when the PR cannot enqueue yet. So a non-null `autoMergeRequest` is not
evidence the PR is queued; it is evidence it is **armed and waiting**, and will enter the queue on
its own once the head refreshes and the requirement is satisfied. Read `isInMergeQueue` for "is it
in the queue" and `autoMergeRequest` for "did something already arm it".
