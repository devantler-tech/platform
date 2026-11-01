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

The private contract probe exports completed results to Coroot. Its pod discovery
annotations select the HTTP `/metrics` endpoint on port 8081. The collection rule
selects the existing Coroot cluster-agent in `observability` and that port. Scraping
reads the last completed observation and does not initiate another public probe.

Use these PromQL queries to distinguish failure reasons and telemetry freshness:

```promql
sum by (result) (increase(contract_probe_requests_total[1h]))
contract_probe_requests_total
contract_probe_failures_total
contract_probe_ready
time() - contract_probe_last_observation_timestamp_seconds
```

Each result series first appears when that result first occurs. `increase()` can
report zero for a failure that happened before its series was first scraped, so
also read the raw per-pod counter. A non-zero raw value shows that the result
occurred since that pod started, but cannot establish when it occurred.

The result counter distinguishes unavailable or invalid responses, configuration
failures, cancellation and concurrent requests. The separate failure counter
records completed unavailable observations using fixed `stage`, `reason` and
`http_status` labels. Stages distinguish request, rejected status and body reads;
reasons classify typed DNS, timeout, TLS, connection and EOF failures. HTTP labels
are bounded numeric statuses or `none`/`other`; raw errors, addresses, headers and
bodies are never exported. These observations narrow the failure mechanism but
do not establish its root cause. Cancelled work publishes no new failure event.
Missing series are an observation gap, not zero failures; a healthy latest sample
does not clear earlier failures or establish long-term reliability.

The standalone probe, like the other workloads, uses signed KHoist 9.0.5, from source
`1f616fea9d63bcb0fac4ea6b27e20dbb80f4b85a`. Its command, readiness behavior,
network boundaries and request limits remain unchanged. The deployment verifier binds the
probe Deployment, current ReplicaSet and Pods to its exact checked-out image and
resolves that image's runtime manifests independently. Controller runtime digests
cannot satisfy the probe check, or vice versa. Rollback reads the rollback
checkout's probe declaration and resolves its immutable image again.

The services CI suite starts the declared image as a non-root user with no
networking, a read-only filesystem and bounded resources. A local read-only
client verifies startup, a completed isolated DNS failure, and two passive metric
scrapes. A skipped or missing image test fails its receipt. This checks the
published executable; live endpoint recovery and Coroot collection remain
production acceptance.

The offline regression suite exercises the actual verifier with controlled
resource and transport responses, including invalid observations and private
canaries:

```bash
bash scripts/tests/test-data-product-rollout.sh
```
