# Observability

`kube-prometheus-stack` (Prometheus + Alertmanager + node-exporter +
kube-state-metrics + Grafana) plus **Loki** (logs), **Alloy** (log
shipper) and **OpenCost** (cost), running per cluster. Self-hosted, no
SaaS metrics tier. The stack is production-hardened along four axes:

1. **Alerts go to Slack** via Alertmanager's native `slack_configs`.
2. **A dead-man's-switch** (the always-firing `Watchdog` alert) is pushed
   to an external heartbeat monitor — the one failure mode in-cluster
   alerting can never cover (the whole cluster being down).
3. **State is persistent** — Prometheus, Alertmanager and Loki use Hetzner
   Cloud block volumes in prod, and Velero ships the `monitoring`
   namespace to R2 daily (24 h RPO).
4. **Everything is reachable** behind oauth2-proxy SSO: Grafana,
   Prometheus, Alertmanager and OpenCost.

## Components

| Component          | Role                                                | Persistence (prod)        |
| ------------------ | --------------------------------------------------- | ------------------------- |
| Prometheus         | Metrics + alert evaluation, 14 d / 5 GiB retention  | `hcloud` PVC, 20 Gi       |
| Alertmanager       | Routing, grouping, silences (2 replicas, gossiped)  | `hcloud` PVC, 2 Gi        |
| node-exporter      | Node metrics (DaemonSet)                            | n/a                       |
| kube-state-metrics | Kubernetes object metrics                           | n/a                       |
| Grafana            | Dashboards + log exploration (Prometheus + Loki DS) | ephemeral (provisioned)   |
| Loki               | Log store, single-binary, 7 d retention             | `hcloud` PVC, 10 Gi       |
| Alloy              | Per-node log shipper → Loki (DaemonSet)             | n/a                       |
| OpenCost           | Cost allocation against Prometheus                  | n/a                       |

Local/CI runs the same stack with persistence **off** (emptyDir) — losing
metrics/logs on a restart is fine there. The `hcloud` PVCs are added only
in the hetzner overlay (`k8s/providers/hetzner/.../*/patches/`), the same
way OpenBao gets block storage.

## Alert routing → Slack

Alertmanager sends to a Slack incoming webhook (`slack_configs` with
`api_url_file`). The URL is per-cluster and SOPS-encrypted, so local
clusters get an invalid URL and stay quiet by design.

- `critical` → Slack immediately, repeat every 12 h.
- `warning`  → Slack, repeat every 24 h.
- `critical` inhibits matching `warning` (same alertname/cluster/namespace).

## Dead-man's-switch (off-cluster heartbeat)

In-cluster Alertmanager cannot tell you the cluster is down — it's down
too. To cover that, the chart's always-firing `Watchdog` alert is routed
to a dedicated `heartbeat` receiver that POSTs to an **external** monitor
on a tight cadence (`repeat_interval: 50s`). If the cluster — or the
Prometheus → Alertmanager pipeline — dies, the monitor stops receiving
pings and notifies Slack out-of-band.

Recommended monitor: [healthchecks.io](https://healthchecks.io) (free,
open-source, native Slack integration). Create a check with a ~5 min
period and ~10 min grace, connect it to Slack, and put its ping URL in
`alertmanager_heartbeat_url` (below). A self-hosted alternative is a
scheduled GitHub Actions workflow that probes the public Gateway and posts
to Slack — fully under your control, no third-party monitor.

The heartbeat URL is injected by Flux substitution
(`${alertmanager_heartbeat_url}`); unset, it defaults to an invalid URL,
so local/CI simply never heartbeat — harmless.

## Off-cluster metric/log backup

There is no remote-write or SaaS mirror. Instead, the persistent
Prometheus, Alertmanager and Loki volumes live in the `monitoring`
namespace, which Velero's `daily-full` schedule backs up to R2 every day
(`includedNamespaces: ["*"]`, Kopia fs-backup). Restore is the standard
Velero flow in [runbook.md](./runbook.md). Backups are filesystem-level
and crash-consistent (Prometheus/Loki recover via their WAL on restore);
fine for a 24 h RPO.

## Grafana

Self-hosted, exposed at `grafana.${domain}` behind oauth2-proxy SSO. Since
the route is already gated to a single GitHub user, Grafana runs with
anonymous **Admin** and the login form disabled — whoever clears the SSO
gate is the operator. Datasources: Prometheus (auto-wired by the chart)
and Loki. Default Kubernetes dashboards are provisioned; the pod stays
ephemeral because dashboards are config, not state.

## What gets alerted

Two sources:

1. **Curated chart default rules** (`defaultRules.create: true`). We keep
   the well-tested groups — `general` (incl. `Watchdog`), `alertmanager`,
   `prometheus`, `prometheusOperator`, `kubernetesApps`
   (CrashLooping/ReplicasMismatch/…), `kubernetesStorage`, `node`,
   `kubeStateMetrics` — and disable the groups for control-plane
   components we don't scrape (etcd, kube-apiserver/-scheduler/-controller-
   manager, kube-proxy, windows). `KubeCPUOvercommit` / `KubeMemoryOvercommit`
   are disabled — guaranteed noise on a cluster that runs hot on purpose.

2. **Platform-specific rules** in
   `k8s/bases/infrastructure/alerts/platform-critical.yaml` (not in the
   chart): Velero/CNPG backups, Flux reconciliation, cert-manager expiry,
   cluster-autoscaler and resource-pressure.

| Alert                       | Severity | Why                                       |
| --------------------------- | -------- | ----------------------------------------- |
| `Watchdog`                  | none     | Always firing → external heartbeat        |
| `NodeNotReady`              | critical | Single node loss                          |
| `PersistentVolumeFillingUp` | critical | >90% PVC                                  |
| `CertificateExpiringSoon`   | warning  | <14 d to expiry, cert-manager not renewing |
| `FluxKustomizationNotReady` | critical | Reconciliation broken >15 min             |
| `VeleroNoRecentBackup`      | critical | RPO breach — no successful backup in 30h  |
| `CNPGClusterDegraded`       | critical | Primary alone, no streaming replica       |

(plus the chart's workload/storage/self-monitoring alerts.)

## Per-environment setup (manual SOPS steps)

The Slack webhook and heartbeat URL are secrets, so they live in the
per-cluster `variables-cluster-secret.enc.yaml` and must be set by hand
(the agent cannot edit `*.enc.yaml`). Both are read from the `Secret`
`variables-cluster`, which is a Flux `substituteFrom` source.

```bash
# 1. Slack incoming webhook for alert notifications.
sops --set '["stringData"]["alertmanager_webhook_url"] "https://hooks.slack.com/services/XXX/YYY/ZZZ"' \
  k8s/clusters/prod/variables/variables-cluster-secret.enc.yaml

# 2. External heartbeat-monitor ping URL (e.g. healthchecks.io).
sops --set '["stringData"]["alertmanager_heartbeat_url"] "https://hc-ping.com/<uuid>"' \
  k8s/clusters/prod/variables/variables-cluster-secret.enc.yaml
```

Slack side: create an incoming webhook for the `#platform-alerts` channel
(the channel in the config is cosmetic — an incoming webhook posts to the
channel it was created for). healthchecks.io side: create the check,
connect its Slack integration, copy the ping URL.

| Env   | `alertmanager_webhook_url`        | `alertmanager_heartbeat_url`       |
| ----- | --------------------------------- | ---------------------------------- |
| local | invalid URL (alerts stay local)   | unset → invalid (no heartbeat)     |
| prod  | Slack `#platform-alerts` webhook  | healthchecks.io ping URL           |

## On-call: silence and inspect

- **Silence an alert** while you work: Alertmanager UI at
  `https://alertmanager.${domain}` → Silences → New.
- **Check why an alert fired / query metrics**: Prometheus at
  `https://prometheus.${domain}` (Graph / Alerts / Targets), or a Grafana
  dashboard at `https://grafana.${domain}`.
- **Read logs**: Grafana → Explore → Loki datasource, e.g.
  `{namespace="velero"} |= "error"`.
- **Cost**: OpenCost at `https://opencost.${domain}`.

All four are behind GitHub SSO (oauth2-proxy, `devantler` only).

## Resource footprint (prod)

| Component      | Requests          | Limits      |
| -------------- | ----------------- | ----------- |
| Prometheus     | 50m / 256 Mi      | — / 1.5 Gi  |
| Alertmanager   | 50m / 64 Mi (×2)  | — / 128 Mi  |
| Grafana        | 50m / 128 Mi      | — / 256 Mi  |
| Loki           | 50m / 128 Mi      | — / 512 Mi  |
| Alloy          | 25m / 96 Mi (×node) | — / 256 Mi |
| Operator       | 50m / 128 Mi      | — / 256 Mi  |

VPA right-sizes the requests at runtime (RequestsOnly), so the limits are
the real ceilings. The always-on tier (Grafana, Loki, persistent
Prometheus + Alertmanager) is what motivates the planned bump to a 4th
static worker (`ksail.prod.yaml`); it's held at 3 today while prod is
right-sized, and the hetzner overlay opts out of kubevirt/cdi to free
~1.5 GiB so the tier still fits.

## Related

- [DR runbook](./runbook.md) — what to do when an alert fires, and restore
- [Velero + CNPG](./velero-cnpg.md) — the systems whose health is checked
- [restore-drill.md](./restore-drill.md) — CI validation of the stack
- [HA primitives](../../README.md) — cluster environments and topology
