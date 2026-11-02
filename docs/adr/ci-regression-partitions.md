# Partition complete offline CI regressions

## Decision

Run the complete cluster-policy regressions as explicit matrix suites in
`ci.yaml`. Each suite receives an isolated checkout and the same pinned tools and
read-only token. At most three suites run concurrently. Fail-fast is disabled, so
a failed suite does not cancel the other diagnostics.

Cluster policies have fixtures, tenant, services, placement, storage and
observability suites. Every validation step belongs to exactly one declared
suite. The publication-generator suite runs in its own required job with the same
checkout, compiler and native tool setup. Repository guards and independent guards
run in two fixed independent jobs, each with its own checkout and the same pinned
compiler and native kubectl. The independent job also runs the existing native
gateway receiver proof, alongside the complete Go regression job. The proof keeps
its original command, explicit enablement, eight-minute limit, pinned image,
restricted container, physical-peer cases and logical-peer mutation control.
Both guard jobs retain unconditional steps, including the checks
that independently protect CI wiring. Required CI and production deployment wait
for both jobs, and the aggregate explicitly refuses every non-success publication
guard result. Making independent guards depend on repository guards and complete
Go regressions is the serial diagnostic control; their commands and inputs stay identical.
Regressions that share one shell environment remain in the same step.

Move the independent OpenBao standby, Crossplane egress, native-mail, Kyverno
review-admission, unscanned-namespace, publisher-pin and Coroot application-health
regressions out of the serial manifest job into one required PR matrix. Preserve
their complete steps, timeouts and Kubernetes-path selection. Copy the pinned
compiler, YAML parser, Kyverno, KSail and schema-cache restore prerequisites into
the isolated checkout. This job has only read access to contents; SARIF upload
remains in the existing manifest job. A distinct candidate cache key restores
from the existing schema prefix; cached data never substitutes for evaluation.

The manifest matrix has two groups. `contracts` retains Coroot application
health, standby repair, Crossplane egress, Kyverno review admission, unscanned
namespaces and the shared publisher pin. `native` retains static snapshot
readback, isolated recovery, mail credential custody, account and recipient
restrictions, and gateway identity. Both get their own runner and the complete
pinned prerequisites. At most two run concurrently; fail-fast is disabled.
Setting this matrix's `max-parallel` to one is the equivalent serial diagnostic
control. Neither setting changes the eleven validation steps or their deadlines.

The existing selection predicates remain authoritative. The required aggregate
consumes the complete matrix result and refuses failed, cancelled, missing or
unexpectedly skipped selected validation. It explicitly requires generator
success. Production deployment waits for both the generator and its existing
merge-group prerequisites.
The independent manifest-regression family remains PR-only, like its original
execution. Required CI accepts success when selected and an intentional skip
otherwise, rejecting unknown selection and every selected non-success result.

The rollout regression's four snapshot-mutation families use two test-only
workers. Each case owns its directory and reads an immutable seed. Foreground
cases drain the workers first; shared policy changes have an explicit barrier.
Every assigned case must return successfully and leave its exact completion
receipt. Interruption, failure, missing receipts and duplicate assignments fail
the suite, and cleanup joins the workers before removing shared scratch. The
production verifier and all fixture assertions and real deadlines stay intact.

## Evidence and alternatives

Successful protected run [41312932796](https://github.com/devantler-tech/platform/actions/runs/41312932796)
spent 675 seconds in repository guards before deployment started. The seven
contiguous publication-report, consumer-discovery, signer-membership, regeneration
and diagnostic steps accounted for 260 seconds. They motivate a separate fixed
publication-guard job while the other 56 checks retain their existing job and
order. This is an observational partition choice, not a predicted or demonstrated
pipeline speedup; duplicate setup, cold compiler caches and runner waiting remain
part of candidate and protected-delivery acceptance under #5495.

Fresh successful protected runs
[41340025399](https://github.com/devantler-tech/platform/actions/runs/41340025399),
[41337419169](https://github.com/devantler-tech/platform/actions/runs/41337419169)
and [41336495029](https://github.com/devantler-tech/platform/actions/runs/41336495029)
show the complete Go job gating deployment at 594, 636 and 610 seconds. Its
native gateway proof follows the ordinary suite and takes 111, 115 and 111
seconds respectively. It consumes the checkout and pinned tools rather than
outputs of that earlier suite, so the independent guard job now runs it first.
The root suite keeps its eight-worker limit and all deadlines. A current local
four-CPU, Go 1.26.10 baseline ran all 587 credential-bridge cases with no failures
or skips in 309.66 seconds; this is profiling evidence, not a candidate speedup.
Hosted validation and the full deployment path must establish the actual effect.

The later [complete green run](https://github.com/devantler-tech/platform/actions/runs/40848728705)
spent 10m14s in independent manifest regressions and 8m47s in complete Go
regressions. Its six contract steps accounted for 305 seconds and its five
native steps for 275 seconds. These observed step totals motivate two balanced,
isolated groups alongside the bounded Go test concurrency. They do not predict
runner availability or demonstrate a faster complete pipeline; candidate and
protected-merge measurements remain required under #5495.

Successful run [40272630712](https://github.com/devantler-tech/platform/actions/runs/40272630712)
took 17m25s for repository guards and 21m06s for cluster policies. The generator
controls occupied 7m48s of the serial guard job. Manifest validation took 14m14s;
its unchanged duration and shared runner queues still bound overall feedback.

The first [scheduling trial](https://github.com/devantler-tech/platform/actions/runs/40289350541)
reduced the guard job to 9m32s and completed its longest policy suite in 12m09s.
That attempt failed because existing validators assumed the old job locations;
it is not evidence of a successful complete feedback loop. Its manifest job took
17m22s. OpenBao standby, Crossplane egress and native mail accounted for 134s,
143s and 129s respectively, motivating the additional independent job. The
remaining validators now check the new owning jobs and their required
dependencies, retaining the existing lost-command and bypass controls.

The next [complete green trial](https://github.com/devantler-tech/platform/actions/runs/40293296500)
took 17m12s including queue and setup. Manifest validation took 11m52s and its
parallel regression job 6m23s, leaving room to rebalance four unchanged checks.
The services policy suite took 12m11s; the rollout regression alone consumed
11m10s. These observations motivate the bounded fixture workers and rebalancing;
the complete feedback target remains unproven until another native run finishes.

The [bounded-worker trial](https://github.com/devantler-tech/platform/actions/runs/40300652085)
finished in 9m57s, but failed the executable-bit guard because the new worker
test was tracked as mode `100644`. Its services suite passed in 8m02s; both
manifest jobs passed in 9m18s. The mode is corrected to `100755`, and both
executable guards pass. This failed attempt is timing evidence, not successful
feedback acceptance. The follow-up must pass every native gate at its own head.

[GitHub's matrix documentation](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/run-job-variations)
defines independent job combinations, fail-fast and concurrency limits. The
inference is that overlapping independent suites can shorten the critical path;
only a complete first-attempt workflow measurement establishes the actual result.
The measurement includes setup and runner queue time.

Running all scripts concurrently inside one checkout would require proving that
their temporary files, environment changes and fixture mutations never interfere.
Separate hosted jobs provide that isolation. The narrowly scoped rollout workers
instead prove distinct case directories and immutable inputs, with barriers for
shared changes and serial foreground cases. Caching prior verdicts, omitting
tests and increasing timeouts do not meet the feedback or coverage requirement.

## Verification and recovery

`scripts/tests/test-policy-ci-job.sh` checks the authored partition and exercises
missing, duplicate and unknown suites, unselected and repeated regressions,
ignored failures and premature cancellation. The generator and existing policy
result controls exercise success and every selected non-success result.
The additional manifest-regression boundary exercises both one- and two-worker
routing, all twelve named checks, invalid worker bounds, selection, success,
failure, cancellation, missing results and unexpected skips. The step inventory
must preserve commands, environments, pinned actions, deadlines, permissions
and prerequisites against the reviewed baseline.
The fixed guard-family boundary exercises independent and serial dependency
shapes, lost jobs and dependencies, conditional or tolerated checks, unexpected
prerequisites, write-permission drift, and every non-success aggregate result.
It also refuses a missing, duplicated, disabled, filtered or wrongly owned native
receiver proof. Runtime proof remains the real isolated container test, including
its negative mutation; workflow-shape checks do not substitute for that execution.
The KSail startup regression renders both providers and their Flux dependency
graph. It keeps the OIDC-dependent release alongside the Gateway, its signed
source and namespace prerequisites earlier, production prune protection and
authentication intact, and local KSail disabled unless explicitly opted in.
`scripts/tests/test-fixture-workers.sh` proves actual overlap, complete receipts,
serial operation and rejection of failures, early errors, duplicate or unknown
cases, missing, corrupt or unassigned receipts, interruption and invalid worker
bounds. A failing peer must still be joined with its healthy sibling before
cleanup returns; serial mode also checks the observed concurrency bound.

Before accepting a scheduling edit, compare its complete command inventory and
step inputs with the reviewed baseline. A passing partition check establishes
complete routing of the authored steps; it does not prove that a deleted command
was an authorized removal. Review and native CI retain that responsibility.

Observe every matrix suite and the required aggregate at the candidate and merged
source. Record actual durations on #5362; a green workflow does not by itself
prove the 15-minute target. Recovery restores serial scheduling through a reviewed
change while retaining the same validation commands and required-result gates.
