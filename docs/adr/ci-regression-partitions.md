# Partition complete offline CI regressions

## Decision

Run the complete cluster-policy regressions as explicit matrix suites in
`ci.yaml`. Each suite receives an isolated checkout and the same pinned tools and
read-only token. At most three suites run concurrently. Fail-fast is disabled, so
a failed suite does not cancel the other diagnostics.

Cluster policies have fixtures, tenant, services, placement, storage and
observability suites. Every validation step belongs to exactly one declared
suite. The publication-generator suite runs in its own required job with the same
checkout, compiler and native tool setup. The other repository guards retain their
unconditional steps, including the checks that independently protect CI wiring.
Regressions that share one shell environment remain in the same step.

Move the independent OpenBao standby, Crossplane egress, native-mail, Kyverno
review-admission, unscanned-namespace, publisher-pin and Coroot application-health
regressions out of the serial manifest job into one required PR job. Preserve
their complete steps, timeouts and Kubernetes-path selection. Copy the pinned
compiler, YAML parser, Kyverno, KSail and schema-cache restore prerequisites into
the isolated checkout. This job has only read access to contents; SARIF upload
remains in the existing manifest job. A distinct candidate cache key restores
from the existing schema prefix; cached data never substitutes for evaluation.

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
The additional manifest-regression boundary exercises selection, success,
failure, cancellation, missing results and unexpected skips.
`scripts/tests/test-fixture-workers.sh` proves actual overlap, complete receipts,
serial operation and rejection of failures, early errors, duplicate or unknown
cases, missing or corrupt receipts, interruption and invalid worker bounds.

Before accepting a scheduling edit, compare its complete command inventory and
step inputs with the reviewed baseline. A passing partition check establishes
complete routing of the authored steps; it does not prove that a deleted command
was an authorized removal. Review and native CI retain that responsibility.

Observe every matrix suite and the required aggregate at the candidate and merged
source. Record actual durations on #5362; a green workflow does not by itself
prove the 15-minute target. Recovery restores serial scheduling through a reviewed
change while retaining the same validation commands and required-result gates.
