# ADR: Keep one production proof per production change, and stop the delivery lane waiting

- **Status:** Accepted on 2026-11-13. Step 1 is in progress under
  [#5077](https://github.com/devantler-tech/platform/issues/5077); nothing here is deployed yet.
- **Decision:** Every change that alters a production input is still deployed to production and
  verified, alone and in queue order, before it merges. A change that alters no production input no
  longer takes the production lane. The organisation's pull-request checks are trimmed where that
  costs no proof, so the lane's jobs find a free runner.
- **Tracks:** [#6203](https://github.com/devantler-tech/platform/issues/6203).

## Context

### How a change reaches `main`

`main` is gated by a merge queue. Its ruleset is managed in `devantler-tech/.github`
(`deploy/repository-rulesets/require-merge-queue-on-platform.yaml`) and read back live on
2026-11-13:

| Setting | Value |
| --- | --- |
| Merge method | Squash |
| Only merge non-failing pull requests | On: every queued change must pass its own checks |
| Build concurrency | 2 |
| Group size | 1 minimum, 1 maximum |
| Wait for the minimum group size | 5 minutes (never reached with a minimum of 1) |
| Status check timeout | 120 minutes |

A queued change runs the `merge_group` event of `ci.yaml`: about 21 jobs. When the change matches
the `k8s` path filter the run also deploys the candidate to production, before it merges:

1. Validation jobs run in parallel.
2. `Wait for the production deployment turn` holds the candidate until every change ahead of it is
   on `main`.
3. `Deploy to Prod` publishes and reconciles the candidate, then runs the Talos update and the
   production checks. One deploy runs at a time, under the `prod-deploy` lock.
4. `CI - Required Checks` reports the result, and the queue merges the change.

A failed deploy evicts the change, and `Heal Prod` redeploys `main` under the same lock. Since
[#6036](https://github.com/devantler-tech/platform/pull/6036) (2026-11-11) the second queued change
validates while the first deploys.

### What was measured

All 930 `merge_group` runs of `ci.yaml` created from 2026-10-17 to 2026-11-13, the 800 pull
requests behind them, and every workflow job in nine organisation repositories from 2026-11-12
00:00 UTC to 2026-11-13 11:00 UTC. Job start and end times come from GitHub's own check-run
records. A job's wait is its start time minus the end of the last job it depends on. For a job
that takes the `prod-deploy` lock, that wait is split into the time another job held the lock and
the time after it.

### Where a queued change spends its time

Successful runs that deployed (633 over the 27 days):

| Stage | Median | 90th percentile |
| --- | ---: | ---: |
| Run created → validation finished | 10.8 min | 21.8 min |
| Deploy job | 14.6 min | 19.4 min |
| Run created → required check reported | 28.0 min | 45.7 min |

Runs that did not deploy (162) reported in 11.3 minutes at the median. Eight in ten merges deploy.

How fast the queue moves while it is backlogged, measured from one merge to the next:

| Period | Median time per merge | What changed |
| --- | ---: | --- |
| 2026-10-17 to 2026-11-02 | 27.2 min | Validation and deploy ran one after the other |
| 2026-11-12 | 13.5 min | #6036: validation overlaps the deploy ahead |
| 2026-11-13 | 38.8 min | Jobs waited for runners |

Time from queueing to merging followed: 25 minutes at the median on 2026-11-12 across 45 merges,
and 286 minutes on 2026-11-13.

### The binding constraint is runners, not the queue

Between two production deploys three jobs each need a fresh runner, one after the other:

1. The required check of the change that just deployed. It holds no lock and needs no approval, so
   its wait is runner time only.
2. `Observe Production Convergence` in `validate-main.yaml`. It starts on every push to `main`,
   takes the `prod-deploy` lock and keeps it while it waits for a runner. Its work takes 0.6
   minutes.
3. The deploy job of the next change. It waits for that lock, and then for a runner of its own.

| | 2026-11-12 | 2026-11-13 |
| --- | ---: | ---: |
| Required check, wait for a runner | 0.1 min | 8.1 min |
| Observer, wait for a runner while holding the lock | 0.1 min | 10.6 min |
| Next deploy job, total wait | 0.1 min | 13.7 min |
| of which while another job held the lock | 0 min | 9.2 min |
| of which with the lock free, waiting for its own runner | 0.1 min | 6.7 min |
| Deploys queued behind the observer | 17 of 38 | 13 of 14 |
| Deploys queued behind another lock holder | 1 of 38 | 2 of 14 |
| One deploy to the next | 14.3 min (5 to 12 November) | 39.8 min |

Every figure is a median, so the two parts of the deploy wait do not add up to its total. The
last row is the median over consecutive deploys where the next change had already finished
validating: 28 such hand-offs from 2026-11-05 to 2026-11-12, and 7 on 2026-11-13. The `prod`
environment has a branch rule only, with no reviewers and no wait timer, so no part of these waits
is an approval.

The deploy itself did not change: 12.6 minutes at the median on 2026-11-13. In one run of 118
minutes, jobs on the critical path waited 68 minutes before they started, and the deploy took 14.

The organisation's plan allows 60 standard hosted jobs at once. From 2026-11-12 17:00 UTC to
2026-11-13 10:40 UTC the organisation averaged 57.7 running jobs and ran 55 or more for three
quarters of that time.

| Repository | Share of job time | Largest part |
| --- | ---: | --- |
| `ksail` | 54% | System tests on pull requests: 246 runner hours, 23% of all capacity |
| `platform` | 32% | Pull-request CI: 195 runner hours. Queue runs: 52 |
| Seven others | 14% | |

Three smaller facts from the same window:

- About 20% of pull-request runner time in `ksail` and `platform` went to runs that the next push
  cancelled.
- Every `platform` pull request ran the shared Go validation twice, once as the organisation's
  required workflow and once inside `ci.yaml`: about 39 runner hours in 35 hours.
- The same starvation was recorded on 2026-10-05 and 2026-10-06
  ([ksail#7579](https://github.com/devantler-tech/ksail/issues/7579),
  [.github#475](https://github.com/devantler-tech/.github/issues/475)) and is why the queue's
  status check timeout was raised to 120 minutes.

### Four in ten deploys change nothing in production

Of the 633 deploys, 244 published manifests identical to `main`. They came from 243 pull requests:

| What the pull request touched | Pull requests |
| --- | ---: |
| Only tests, documentation or fixtures | 38 |
| Only scripts the deploy never references | 71 |
| The CI or CD workflow file, plus such scripts | 75 |
| Go module files or shared Go packages | 33 |
| A file the deploy action references directly | 26 |

They deployed because one path filter, `k8s`, selects both the validation jobs and the production
deploy, and that filter deliberately lists tests and their fixtures so a change to a test runs the
test. A deploy of unchanged manifests still takes about 10.6 minutes: Flux reconciles to a new
revision with the same content, and the Talos update, the credential bridge and the production
checks all run. A static trace from the deploy action reaches 56 of about 820 non-test script
files.

The arithmetic on a starved day: at 39.8 minutes per deploy the lane handles 36 deploys a day,
while a busy day brings 36 to 45, so the queue only grows.

| What stops deploying | Share of deploys | Deploys on a busy day |
| --- | ---: | ---: |
| Nothing (today) | 0% | 36 to 45 |
| The first list in #5077, replayed over the 629 merged commits | 12% (74) | 32 to 40 |
| Every deploy of unchanged manifests that leaves the deploy itself alone | 34% (218) | 24 to 30 |

The first list keeps every workflow file and the Go module files as production inputs, so on its
own it does not let a starved lane keep up with the busiest days. The last row is the ceiling: the
244 deploys of unchanged manifests, less the 26 whose change touched a file the deploy action
references directly, which keep their deploy. Reaching it needs the later narrowing steps.
Removing the observer's lock from the hand-off is the other half: at about 30 minutes per deploy
the same lane handles about 47 a day.

### Half of the evictions were not the change's fault

There were 98 evictions against 795 merges. 49 of the evicted changes later merged with no new
commit. Of 66 failed deploys, the recovery deploy of `main` also failed in 35. Failed deploys and
recoveries held the production lock for 36 of 198 hours, 18%.
[#4979](https://github.com/devantler-tech/platform/issues/4979) owns this.

## Decision

1. **The proof stays as it is.** One production deploy per production-changing change, in queue
   order, group size 1, every queued change passing its own checks, and a recovery deploy after any
   failure. No change merges with less proof than today for the risk it carries.
2. **A change that alters no production input does not deploy** (#5077). The production inputs are
   declared in one list. The deploy job runs from a working tree that contains only that list, so a
   file outside it cannot influence a deploy, and a dependency the list misses fails that deploy
   loudly. A queued change deploys when anything on the list differs from its base, and also
   whenever the comparison cannot be made. The first delivery keeps every workflow file and the Go
   module files on the list.
3. **Pull-request checks do less runner work where that costs no proof**: run the Go validation
   once ([#6204](https://github.com/devantler-tech/platform/issues/6204)), push a change once per
   round ([monorepo#5009](https://github.com/devantler-tech/monorepo/issues/5009)), and reduce the
   work per pull request in the two repositories that fill the allowance (ksail#7579, .github#475).
4. **The lane is measured** ([#6206](https://github.com/devantler-tech/platform/issues/6206)), with
   two targets: under 45 minutes at the median from queueing to merging on a day with up to 60
   merges, and under 5 minutes at the 90th percentile from one deploy ending to the next starting.
5. **The wait between two deploys is shortened**
   ([#6205](https://github.com/devantler-tech/platform/issues/6205)). First the observer stops
   holding the production lock while a deploy is waiting, which changes no merge proof. The
   deploy and required-check jobs are changed only after step 2, because they are the most
   carefully guarded part of the workflow.

## Alternatives considered

- **One deploy for several queued changes.** GitHub's queue runs one CI run per queued change, each
  on its own commit, whatever the group size. Sharing one deploy means a run must wait for, and
  trust, another run's result, and a failed deploy no longer says which change broke production.
  With 11% of queued changes evicted today, a group of three would fail about three times in ten.
  Revisit if the lane is busy for more than 70% of a week after step 2.
- **Building more queued changes at once.** Validation already finishes inside the deploy ahead,
  on the starved day too. A third candidate would add about 21 jobs to a full pool. Revisit after
  step 2: a change that does not deploy takes a build slot without taking the lane.
- **Proving a change on a separate cluster.** There is no production-like cluster, and the local
  cluster is a manual test-bed by design. A deploy there is weaker proof than today's.
- **Capping how many runners `ksail` system tests may take.** Replaying the measured window with a
  cap of 16 jobs leaves the organisation at 55 or more running jobs for 62% of the time, against
  75% without it. The same work still has to run, so the wait moves and does not shrink.
- **Self-hosted runners for the deploy.** The organisation pool is prepared but inactive and allows
  one job at a time ([arc-runners.md](../operations/arc-runners.md)). A deploy that restarts
  cluster nodes should not run on that cluster. The required check runs no repository code and
  needs no secrets, so it is the first candidate once the pool is active (#6205).
- **A larger runner allowance.** GitHub's documented limits are 60 concurrent standard jobs on the
  current plan and 500 on the next, and larger runners have a separate pool. Either would remove
  the constraint this record works around. Both are purchases, which the maintainer decides; this
  record makes no purchase and does not depend on one.
- **Making the deploy itself faster.** A deploy of unchanged manifests spends about two minutes in
  the Flux reconcile, two in the Talos update, one and a half in the credential bridge and under a
  minute each in the API stability wait, the gateway check and the Coroot check. A real change
  needs all of them. For a change with nothing to deploy, the fix is not to deploy.

## Consequences

- Production runs the revision of the last change that deployed. After a change that does not
  deploy, that revision is older than the tip of `main` while the content is the same. This is
  already true for the two in ten merges that do not match the `k8s` filter.
- A change that adds a new file for the deploy to read must add it to the list, or its own queue
  run fails. That is the intended failure: it lands on the change that introduced the dependency.
- The deploy job no longer sees the whole repository. A deploy step that scans a directory outside
  the list would silently see less, so each step is audited for that when the list is introduced.
- A change that does not deploy still occupies one of the two build slots. If that leaves the lane
  idle while the next production change validates, raise build concurrency then, with numbers.
- Runner starvation remains possible. Steps 2 and 3 make the lane keep up through it and make it
  rarer; they do not reserve runners for production.

## Method

- Runs: `GET /repos/devantler-tech/platform/actions/workflows/ci.yaml/runs?event=merge_group`, in
  windows of at most 1,000 results.
- Jobs: GraphQL `checkRuns` on each run's check suite. `startedAt` equals the REST job's
  `started_at`, the moment a runner picked the job up.
- Time in queue: the last `AddedToMergeQueueEvent` before `mergedAt`, to `mergedAt`.
- Backlogged: the next pull request was already queued when the previous one merged.
- Lock time: the part of a deploy job's wait during which another job held `prod-deploy`, taken
  over the union of every such job's interval. 66 jobs across the workflows take this lock. The
  observer holds it from the creation of its push run until its job completes. Any other holder
  is counted from the moment its job started, because its own time in the queue is not in these
  records; for the three deploy waits that overlapped one (a recovery deploy and two short
  dispatched reads), the lock-free part is therefore an upper bound. Counting them moved neither
  median.
- False eviction: the pull request merged later, and neither its last commit nor a force-push is
  newer than the eviction. The event's `beforeCommit` is not the pull request's head and must not
  be used for this.
- Unchanged manifests: the pull request's file list has nothing under `k8s/` (outside the local
  cluster's overlays), `talos/`, `ksail.prod.yaml` or `.sops.yaml`. Three pull requests with more
  than 100 files were counted as changing manifests.
- Concurrency: an exact sweep over job start and end times, weighted by time.
