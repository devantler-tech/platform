# Kyverno policy-report refresh

Kyverno policy reports are derived state. They should describe the current
resource/policy result, not retain historical failures. This platform relies on
that property for the Policy Reporter compliance dashboard.

## Exemptions must produce a current result

For report-backed validation policies, express an exemption so that the exempt
resource still gets a current result when it may already have one:

- in a `ClusterPolicy`, a false `preconditions` entry emits `result: skip` for
  the same policy, rule and resource;
- in a `ValidatingPolicy`, fold the exemption into the validation expression
  (`variables.exempt || <check>`), which emits `result: pass`. A false
  `matchConditions` entry writes no result to the report;
- `exclude`, a changed `match` or a narrower `matchConstraints` emits no
  replacement result.

A shape that writes no result can leave an older result visible even after
hourly background scans. Issue #2573 reproduced this with
`validate-replica-floor`: eleven June failures remained while unrelated
results in the same reports kept refreshing.
The policy's committed Kyverno test pins the supported exemption shape:

```bash
kyverno test tests/validate-replica-floor --require-tests
```

The test requires a normal singleton, an unset replica count, a scaled-to-zero
workload and a single-replica StatefulSet to fail, two or more replicas to pass,
and every supported exemption form (workload label, pod-template label,
namespace, exact name and wildcard primary name) to emit `pass`.

`kyverno test` counts a fixture row as passing when its rule never evaluated
the resource (`REASON=Excluded`), whatever the row declared, and it drops a
declared resource it never loaded without a row or an error. CI therefore runs
`scripts/validate-kyverno-fixture-evaluation.sh tests`, which fails when a row
declaring `pass` or `fail` is Excluded, when a fixture names a rule that no
policy it loads defines, or when a declared resource produced no row. Declare a
resource the rule should leave alone as `result: skip`.

The stale-report proof also updates an exemption while its resource already has
a stored failure. The same report and resource UIDs must then carry exactly one
fresh `skip` for that rule, with no retained failure. Three unexempted controls
must still have fresh failures. This observes background report replacement,
which a static rule-evaluation test cannot establish.

## Safe rollout and verification

1. Express a new replica-floor exemption in the `exempt` variables of
   `validate-replica-floor.yaml`. Do not add a `matchConditions` entry or
   narrow its match scope.
2. Run the Kyverno test above and the normal local/prod static validation.
3. Let Flux deploy the policy change. Do not patch or delete PolicyReports by
   hand and do not restart the reports controller.
4. Wait for the reports controller's normal background scan (configured for one
   hour), then verify the target rule has no failures:

   ```bash
   kubectl --context=oidc@prod get policyreports.wgpolicyk8s.io -A -o json \
     | jq '[.items[].results[]? | select(.policy == "validate-replica-floor")]
       | group_by(.result)
       | map({result: .[0].result, count: length})'
   ```

The exemption changes only the target policy result. Other results in
the same per-resource report remain controller-owned and untouched.

## Automatic pruning of stale failures

Kyverno 1.18.1 exposes no supported API to prune one arbitrary result when a
policy stops matching because its kind, match block, rule name or policy name
changes. Instead, the `prune-stale-policy-reports` Kyverno `DeletingPolicy`
(`k8s/bases/infrastructure/deleting-policies/`) runs every hour at minute 17 and
deletes a namespaced PolicyReport that holds a `fail`, `warn` or `error` result
older than six hours, provided the same report also holds a result written in
the last two hours. The next background scan recreates the report with only
the results that are still evaluated.

What operators should expect:

- A PolicyReport disappearing and returning within about an hour is this policy
  working, not an incident.
- A current failure is rewritten by every background scan, so it never reaches
  six hours and its report is never deleted. Old `pass` and `skip` results,
  which mutate rules record once at admission, do not trigger deletion.
- A report with no recent result is never deleted by this policy. Background
  scans skip ReplicaSets, so their reports are written at admission and never
  refreshed; their old failures may still be real, so they stay visible. If the reports
  controller stops publishing, failures stay in place once its newest result is
  two hours old; in those first two hours a report can still be deleted, and it
  stays missing until the controller recovers and rescans.
- A result left behind by an `exclude` on a resource that no other rule still
  evaluates is not pruned either. Express the exemption as described above so the
  scan rewrites it as a current result.
- Cluster-scoped `ClusterPolicyReport` objects are not covered.

## Authorization review dispatch

Kyverno's pinned chart already filters the standard TokenReview,
SubjectAccessReview and SelfSubjectAccessReview kinds internally. Its
`config.matchConditions` also excludes exactly those resources in their standard
authentication/authorization API groups before webhook dispatch. This removes a
no-op dependency from aggregated API discovery, which can itself require a
delegated authorization review. Kubernetes still performs the actual token or
permission review; this does not grant the caller any access.

LocalSubjectAccessReview, SelfSubjectRulesReview, same-named resources in other
API groups, workloads, Secrets and RBAC requests remain in webhook scope. The
existing resource filters, namespace selectors and fail-closed policy behavior
are unchanged. `scripts/tests/test-kyverno-auth-reviews.sh` renders the chart's
ConfigMap and exercises its conditions against an isolated Kubernetes API server,
including an unfiltered positive control and rejecting-webhook controls. Live
verification must additionally observe the generated webhook configuration and
separated discovery cycles; a single successful metrics request does not clear
an intermittent failure.

## System namespaces

Background scans honour Kyverno's resource filters (`skipResourceFilters: false`),
so the filters decide which namespaces get reports:

- **kube-system, kube-public and kube-node-lease are scanned.** The platform drops
  them from the chart's default filters (#4234), so a policy that matches a
  resource there gets a current report, as in any other namespace. The webhooks
  exclude kube-system at admission. kube-public and kube-node-lease are within
  the webhook scope: kube-public's bootstrap Role matches the enforcing
  `audit-privileged-rbac` rule, while node Leases remain resource-filtered.
  Creating a different kind there can match other admission policies; these
  namespaces are not blanket enforcement exemptions. Every policy that generates or changes existing resources
  excludes these three namespaces by name, so Kyverno creates and edits nothing
  there.
- **kyverno is not evaluated.** It stays in the filters, because two generate
  policies do not exclude it and would otherwise create a default-deny network
  policy and a LimitRange in Kyverno's own namespace. No report there is ever
  created or rewritten; any report found there predates the filters and shows a
  past result, not a current one.

### Coverage decision and admission review

The decision for #4228 retains the deployed three-namespace background coverage
from #4235 and Kyverno's own self-protection filter. This restores visibility for
matching system resources without sending kube-system or kyverno through the
resource admission webhooks. Removing Kyverno's own filter would let its generate
policies target the policy engine itself, so that namespace remains a declared
coverage gap in Kyverno reports.

The admission review distinguishes selectors from resource filters. Background
coverage does not imply new kube-system admission enforcement. kube-public can
receive enforcing RBAC validation, and its existing Role has a passing result;
future privileged grants there must satisfy the existing reviewed RBAC boundary.
Leases remain filtered. The generate and mutate-existing rules exclude all three
Kubernetes system namespaces, so they do not create or edit resources there. This
decision changes no filter, selector, policy action or reviewed RBAC exclusion.

Read-only verification on 2026-10-03 joined fresh replica-policy results to the
current kube-system Deployments and confirmed the observed Kyverno release and
resource webhook selectors. It establishes the issue's required current report
and the deployed admission scope, not complete coverage of every resource or a
week of reports-controller memory stability. Empty collections and retained June
reports are not compliance evidence.

Kubescape supplies separate posture visibility for Kyverno's workloads, not
equivalent Kyverno admission enforcement. Its stored results lack per-object scan
timestamps and source workload UIDs, so their freshness is unproven; see the
[result coverage contract](kubescape-result-coverage.md). Kubescape excludes the
other three system namespaces and cannot replace their Kyverno background coverage.

A `DeletingPolicy` cannot clear reports in the kyverno namespace either: Kyverno's
cleanup controller skips the namespaces the filters exclude. A policy matching them
runs on schedule and deletes nothing there, while the same condition deletes a
matching report in an ordinary namespace. In the three scanned namespaces, a scan
rewrites only a result that a current rule still evaluates for a resource that
still exists; a report for a deleted resource, or a result from a removed or
renamed rule, needs separate clean-up. The proof in
`scripts/prove-kyverno-stale-report-prune.sh` refuses to run unless its Kyverno
settings match production's, then checks that a resource a policy matches gets a
report in each of the three scanned namespaces and none in kyverno, so this section
fails loudly if a settings or chart change moves either boundary.

An empty report list in the kyverno namespace means it is not covered by Kyverno
at all, not that it is compliant.

Manual whole-report deletion, direct result patches and controller restarts are
still not recovery mechanisms for this platform. If a stale failure survives
the automatic pruning, investigate and track it separately instead of mutating
generated reports.
