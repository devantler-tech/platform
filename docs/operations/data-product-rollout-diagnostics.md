# Data Product rollout diagnostics

The deployment verifier refuses incomplete observations and rollouts that do not
settle within its shared deadline. A failed receipt reports `complete: false` and
keeps the original `failure` classification. Diagnostics do not extend the
deadline, grant a retry, or relax an acceptance check.

For snapshot failures, `phase` identifies `initial_readiness` or
`rollout_stability`. `role` names a fixed resource role or check group, and
`reason` distinguishes:

- `predicate_not_ready`: the last completed snapshot failed that role's check.
- `read_incomplete`: a resource read or projection failed.
- `observation_invalid`: the snapshot could not be evaluated reliably.
- `deadline_exceeded`: the budget expired during a read without a retained
  failed snapshot predicate.

A deadline receipt can retain the last completed predicate while a later read
is still in progress. It identifies an observed obstacle, not the initiating
cause of an incident. `snapshot`, `workloads`, `backends`, `routes`, and
`readiness` describe groups when no narrower completed result exists.

Public-response failures use `phase: public_contract` and their fixed endpoint
role and failure class. A public failure observed against a moving snapshot pair
is discarded. `changed` retains only the verifier's field names when a pair
moved; no object values, resource identities, response bodies, addresses, or
credentials appear in diagnostics.

Read the receipt from the protected deployment job. Investigate the named check
through the authorized read-only evidence path before attempting another
deployment. A healthy observation made later cannot establish the failed run's
cause.

The offline regression suite exercises the actual verifier with controlled
resource and transport responses, including invalid observations and private
canaries:

```bash
bash scripts/tests/test-data-product-ui-rollout.sh
```
