# Cilium endpoint creation timing

The **Read Cilium API Limiter** workflow collects five snapshots of the endpoint
creation limiter from each stable ready Cilium agent that has one. Dispatch it from reviewed
`main` with `confirm=read-production-cilium-api-limiter`. It uses the established
production pipeline identity and serializes with deployments. No listener,
limiter setting, network policy or workload is changed.

The fixed command is `cilium-dbg metrics list --output json --match-pattern
'^cilium_api_limiter_'`. The implementation follows the
[Cilium v1.20.4 CLI](https://github.com/cilium/cilium/blob/v1.20.4/cilium-dbg/cmd/metrics_list.go)
and [limiter observer](https://github.com/cilium/cilium/blob/v1.20.4/pkg/rate/metrics/metrics.go).
The [metric JSON](https://github.com/cilium/cilium/blob/v1.20.4/api/v1/models/metric.go)
omits a value of exactly zero, so a gauge with no `value` key is published as 0,
and any other non-numeric value is refused. Counters must carry a value.
The diagnostic refuses other agent versions. A later version needs a source audit
and matching fixture updates before dispatch.

A limiter series exists only after that agent's first processed request for that
call, and the [metric registry](https://github.com/cilium/cilium/blob/v1.20.4/pkg/metrics/cmd.go)
encodes no match as JSON null. On a null read the diagnostic makes one further
fixed read, of `^cilium_version`, whose series exists from agent start. If that
read finds the series, or the limiter read has well-formed series only for other
calls, the agent counts as unobserved and gets no values. An agent that lacks the
series in all five samples publishes no samples, and the verdict counts it. Gaps
in the published indices show which index it was. When other agents are
complete, the run ends `PARTIAL` (exit 0), publishing their samples; when none is,
it ends `INCONCLUSIVE` (`LIMITER_UNOBSERVED`). That shows
the agent reports no limiter series, which normally means no finished
endpoint-create request since it started; disabling the metric through the agent's
`--metrics` setting would look the same, and this repository sets no such flag.
A failed or empty control read is reported as its own read failure.

Public output contains agent indices, capture times, endpoint-create
mean/estimated processing durations, mean/min/max waiting durations,
in-flight/limit values, rate/burst settings, adjustment factor and the aggregate
processed-request counter. Raw responses and identity fields stay in a private
temporary directory which is removed on exit. No raw artifact is uploaded.

`MEASURED` requires a complete finite metric set for every sample and the same
ready DaemonSet, Pod UID, node name, container ID, image ID and restart count at
both ends. `PARTIAL` requires the same identity at both ends, and each agent either complete
in all five samples or without the series in all five. Other missing metrics, a
failed read, a series that appears or disappears between samples or a changed
identity is `INCONCLUSIVE` (exit 3). An unobserved agent says nothing about its
own timing, so `PARTIAL` describes only the agents it publishes. It is not
evidence of a quiet agent either: series appear when a request finishes, so an
agent whose first request is stuck looks the same, and the slowest agent may be
the one left out. Agent indices
follow sorted Pod names, so they repeat between runs while the agents are
unchanged, and they are not mapped to nodes or Coroot machines.

These gauges summarize limiter observations; they are not per-request traces or
latency percentiles. Agents write them only when a request finishes, so five
identical samples can be days old, and the in-flight value is the count at that
last finish. Only a moving request counter shows fresh requests. Three values are
settings, not observations: `estimated` processing time, and `min` and `max`
waiting time, which by default stay 0 and 60 seconds. Compare waiting and
processing values, counter movement, the finite runtime logs and the same Coroot
incident window before choosing a change. A measured sample alone establishes neither request attribution nor
SLO recovery. Keep the incident open until its original objective recovers.

Offline validation: `bash scripts/tests/test-read-cilium-api-limiter.sh` uses a
fake Kubernetes client, checks the exact reads and privacy boundary, exercises
missing/replaced/unsupported agents and malformed/partial/oversized metrics,
and executes both workflow guards with accepted and rejected inputs.
