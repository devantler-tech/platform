# Cilium endpoint creation timing

The **Read Cilium API Limiter** workflow collects five snapshots of the endpoint
creation limiter from each stable ready Cilium agent. Dispatch it from reviewed
`main` with `confirm=read-production-cilium-api-limiter`. It uses the established
production pipeline identity and serializes with deployments. No listener,
limiter setting, network policy or workload is changed.

The fixed command is `cilium-dbg metrics list --output json --match-pattern
'^cilium_api_limiter_'`. The implementation follows the
[Cilium v1.20.3 CLI](https://github.com/cilium/cilium/blob/v1.20.3/cilium-dbg/cmd/metrics_list.go)
and [limiter observer](https://github.com/cilium/cilium/blob/v1.20.3/pkg/rate/metrics/metrics.go).
The diagnostic refuses other agent versions. A later version needs a source audit
and matching fixture updates before dispatch.

Public output contains opaque agent indices, capture times, endpoint-create
rolling mean/estimated processing durations, mean/min/max waiting durations,
in-flight/limit values, rate/burst settings, adjustment factor and the aggregate
processed-request counter. Raw responses and identity fields stay in a private
temporary directory which is removed on exit. No raw artifact is uploaded.

`MEASURED` requires a complete finite metric set for every sample and the same
ready DaemonSet, Pod UID, node name, container ID, image ID and restart count at
both ends. Missing metrics, a failed read or a changed identity is
`INCONCLUSIVE` (exit 3). There is no successful partial result. Opaque indices
are local to one run and cannot be joined to another run or to Coroot machines.

These gauges summarize limiter observations; they are not per-request traces or
latency percentiles. Compare waiting and processing values, counter movement,
the finite runtime logs and the same Coroot incident window before choosing a
change. A measured sample alone establishes neither request attribution nor
SLO recovery. Keep the incident open until its original objective recovers.

Offline validation: `bash scripts/tests/test-read-cilium-api-limiter.sh` uses a
fake Kubernetes client, checks the exact reads and privacy boundary, exercises
missing/replaced/unsupported agents and malformed/partial/oversized metrics,
and executes both workflow guards with accepted and rejected inputs.
