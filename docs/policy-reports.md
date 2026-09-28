# Kyverno policy-report refresh

Kyverno policy reports are derived state. They should describe the current
resource/policy result, not retain historical failures. This platform relies on
that property for the Policy Reporter compliance dashboard.

## Exemptions must produce a current result

For report-backed validation policies, prefer a false `preconditions` entry for
an exemption instead of `rule.exclude` when the exempt resource may already have
a result:

- a false precondition emits `result: skip` for the same policy, rule and
  resource;
- `exclude` or a changed `match` emits no replacement result.

The second shape can leave an older result visible even after hourly background
scans. Issue #2573 reproduced this with `validate-replica-floor`: eleven June
failures remained while unrelated results in the same reports kept refreshing.
The policy's committed Kyverno test pins the supported exemption shape:

```bash
kyverno test tests/validate-replica-floor --require-tests
```

The test requires a normal singleton to fail, two replicas to pass, and every
supported exemption form (workload label, pod-template label, namespace, exact
name and wildcard primary name) to emit `skip`.

`kyverno test` counts a fixture row as passing when its rule never evaluated
the resource (`REASON=Excluded`), whatever the row declared, and it drops a
declared resource it never loaded without a row or an error. CI therefore runs
`scripts/validate-kyverno-fixture-evaluation.sh tests`, which fails when a row
declaring `pass` or `fail` is Excluded, when a fixture names a rule that no
policy it loads defines, or when a declared resource produced no row. Declare a
resource the rule should leave alone as `result: skip`.

## Safe rollout and verification

1. Express a new replica-floor exemption as a precondition in
   `validate-replica-floor.yaml`. Do not add a new `exclude` entry.
2. Run the Kyverno test above and the normal local/prod static validation.
3. Let Flux deploy the policy change. Do not patch or delete PolicyReports by
   hand and do not restart the reports controller.
4. Wait for the reports controller's normal background scan (configured for one
   hour), then verify the target rule has no failures:

   ```bash
   kubectl --context=admin@prod get policyreports.wgpolicyk8s.io -A -o json \
     | jq '[.items[].results[]? | select(.policy == "validate-replica-floor")]
       | group_by(.result)
       | map({result: .[0].result, count: length})'
   ```

The precondition changes only the target policy/rule result. Other results in
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
  evaluates is not pruned either. Use a precondition for the exemption (see
  above) so the scan rewrites it as a current `skip`.
- Cluster-scoped `ClusterPolicyReport` objects are not covered.

## System namespaces

Background scans honour Kyverno's resource filters (`skipResourceFilters: false`),
so the filters decide which namespaces get reports:

- **kube-system, kube-public and kube-node-lease are scanned.** The platform drops
  them from the chart's default filters (#4234), so a policy that matches a
  resource there gets a current report, as in any other namespace. Admission there
  is unchanged in effect: the webhooks never send kube-system objects to Kyverno,
  and kube-public and kube-node-lease hold only ConfigMaps and node Leases, which no
  enforcing rule matches. Every policy that generates or changes existing resources
  excludes these three namespaces by name, so Kyverno creates and edits nothing
  there.
- **kyverno is not evaluated.** It stays in the filters, because two generate
  policies do not exclude it and would otherwise create a default-deny network
  policy and a LimitRange in Kyverno's own namespace. No report there is ever
  created or rewritten; any report found there predates the filters and shows a
  past result, not a current one.

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
