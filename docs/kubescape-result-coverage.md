# Kubescape result coverage

Kubescape stores its findings as objects in the cluster, one set per surface. A workload with no
result object reads exactly like a workload with nothing wrong, so "complete coverage" needs a
fixed answer to *complete coverage of what?* This page is that answer (#4262, part of #3156), and
[`scripts/check-kubescape-result-coverage.sh`](../scripts/check-kubescape-result-coverage.sh) is
the read-only check that compares the live results against it.

## Scope

A namespace is **in scope** unless it is listed in
[`scripts/kubescape-unscanned-namespaces.tsv`](../scripts/kubescape-unscanned-namespaces.tsv). That
is the same reviewed list `scripts/guard-kubescape-scan-scope.sh` ties to the operator's
`excludeNamespaces`, so the expected set and the scanner's own scope can only move together, in a
reviewed change. The check does not keep a second exclusion list.

## Expected set per surface

| Surface | Expected object | Result object | Missing | Stale |
| --- | --- | --- | --- | --- |
| Posture | every Deployment, StatefulSet, DaemonSet and CronJob | a `workloadconfigurationscan` for that workload, carrying control results | no result, or a result with no controls | not observable (see below) |
| Vulnerability | every image a long-lived regular container, init sidecar or ephemeral container is running, keyed by image digest | a `vulnerabilitymanifestsummary` for that digest | no result for the digest | scanned more than 7 days ago |
| Runtime | every running ReplicaSet, StatefulSet revision and DaemonSet revision | an `applicationprofile` **and** a `networkneighborhood`, both `completed/complete` | either half absent | still learning after its learning period |

Why each set is drawn where it is:

- **Posture uses the workload kinds, not a control count.** Earlier surveys counted controls and
  disagreed with each other, because the number of controls reported depends on the object kind
  and on which frameworks the operator loaded. The unit that can be missing is the per-workload
  result, so that is what is counted. Each result must also carry control results, since an
  object with no controls is the "scanned but evaluated nothing" case. Jobs are not expected: the
  operator scans CronJobs, not the Jobs they create.
- **Vulnerability is keyed by image digest.** Ten replicas of one image need one scan, and two
  containers pinned to the same digest share it. Counting containers inflates the denominator
  and makes coverage depend on replica counts. A long-lived container is a regular container, a
  running init sidecar, or a running ephemeral container whose pod is not owned by a Job; that
  includes operator-managed pods such as CloudNativePG instances, which run for as long as a
  Deployment's pods do. Completed one-shot init containers are outside the live expected set. If
  any included container has no SHA-256 image identity, the check returns `UNKNOWN`; it never drops
  an unidentified image from the expected set.
- **Runtime expects the pair.** The node agent needs both profiles to judge a container's
  behaviour, so one half on its own is a gap. A `partial` completion means the agent started
  watching after the container did, so the profile does not describe the whole container; the
  check reports it as a failure. A profile still learning inside its learning period (24 hours by
  default, read from the profile's own label) is `PENDING`, not a failure.

### What the check cannot see

The posture objects carry no per-object timestamp: `creationTimestamp` is the first scan, and the
object is updated in place afterwards. So a posture result that exists but was not refreshed by
the latest scan cannot be told apart from a current one here. Posture freshness is watched at the
scanner instead, by the data age of the whole surface.

## Relationship to the 95% floor

The CI gate (`ksail workload scan --framework nsa,mitre --compliance-threshold 95` in
`.github/workflows/ci.yaml`) and this check answer different questions, and neither replaces the
other:

- The **floor** scores the rendered manifests with the platform's exceptions applied. It says
  whether the posture that *is* evaluated is good enough.
- The **coverage check** says whether every workload the floor scores also has a live result. It
  computes no score, so it cannot raise or lower the floor's number.

The posture expected set is the workload part of what the CI scan scores. RBAC objects, Services
and other non-workload kinds are scored by the floor but are not in this set. A live posture figure
is only comparable to the floor when this check passes. With a gap, the live figure describes a
smaller population than the one the floor scored. The live objects are also pre-exception (their
`appliedIgnoreRules` is empty), so a live score read off them is never the floor's number, even
at full coverage.

## Running it

It needs read access to the cluster and issues only `kubectl get`:

```bash
scripts/check-kubescape-result-coverage.sh --context <kube-context>
```

It prints one `MISSING`, `STALE`, `PARTIAL` or `EMPTY` line per failure, `PENDING` and `ORPHAN`
lines for information, and one `COVERAGE` line per surface. It exits `0` when every expected
object has a current result, `1` when any is missing or stale, and `2` when it cannot check. An
empty read of any input, or posture objects that read back short, is `2`, never a pass.

The aggregated storage API returns `.spec` as null on a LIST, which reads exactly like "no
controls". So the check lists the posture objects, then reads the workload-kind ones back by name
in batches. Only the four workload kinds are read back, which keeps a run to about 20 seconds.

CI has no cluster, so it runs only the fixture test
(`scripts/tests/test-check-kubescape-result-coverage.sh`). The test includes the negative control:
one expected result removed from a passing set must fail.

## First measurement (2026-09-30)

| Surface | Expected | Current | Failing |
| --- | --- | --- | --- |
| Posture | 96 | 96 | 0 |
| Vulnerability | 77 | 55 | 8 missing, 14 stale |
| Runtime | 77 | 70 | 2 (one missing pair and one partial pair), plus 10 profiles still learning |

The remaining vulnerability and runtime gaps are the work of #4263 and #4264.
