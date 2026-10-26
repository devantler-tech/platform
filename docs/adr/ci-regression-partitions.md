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

The existing selection predicates remain authoritative. The required aggregate
consumes the complete matrix result and refuses failed, cancelled, missing or
unexpectedly skipped selected validation. It explicitly requires generator
success. Production deployment waits for both the generator and its existing
merge-group prerequisites.

## Evidence and alternatives

Successful run [40272630712](https://github.com/devantler-tech/platform/actions/runs/40272630712)
took 17m25s for repository guards and 21m06s for cluster policies. The generator
controls occupied 7m48s of the serial guard job. Manifest validation took 14m14s;
its unchanged duration and shared runner queues still bound overall feedback.

[GitHub's matrix documentation](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/run-job-variations)
defines independent job combinations, fail-fast and concurrency limits. The
inference is that overlapping independent suites can shorten the critical path;
only a complete first-attempt workflow measurement establishes the actual result.
The measurement includes setup and runner queue time.

Running all scripts concurrently inside one checkout would require proving that
their temporary files, environment changes and fixture mutations never interfere.
Separate hosted jobs provide that isolation. Caching prior verdicts, omitting
tests and increasing timeouts do not meet the feedback or coverage requirement.

## Verification and recovery

`scripts/tests/test-policy-ci-job.sh` checks the authored partition and exercises
missing, duplicate and unknown suites, unselected and repeated regressions,
ignored failures and premature cancellation. The generator and existing policy
result controls exercise success and every selected non-success result.

Before accepting a scheduling edit, compare its complete command inventory and
step inputs with the reviewed baseline. A passing partition check establishes
complete routing of the authored steps; it does not prove that a deleted command
was an authorized removal. Review and native CI retain that responsibility.

Observe every matrix suite and the required aggregate at the candidate and merged
source. Record actual durations on #5362; a green workflow does not by itself
prove the 15-minute target. Recovery restores serial scheduling through a reviewed
change while retaining the same validation commands and required-result gates.
