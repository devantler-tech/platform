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

## Safe rollout and verification

1. Express a new replica-floor exemption as a precondition in
   `validate-replica-floor.yaml`. Do not add a new `exclude` entry.
2. Run the Kyverno test above and the normal local/prod static validation.
3. Let Flux deploy the policy change. Do not patch or delete PolicyReports and
   do not restart the reports controller.
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

## Limitation

Kyverno 1.18.1 exposes no supported API to prune one arbitrary result when a
policy stops matching because its kind, match block, rule name or policy name
changes. `CleanupPolicy` deletes whole Kubernetes resources, not individual
report results. Whole-report deletion, direct result patches and controller
restarts are therefore not recovery mechanisms for this platform. Investigate
and track those cases separately instead of mutating generated reports.
