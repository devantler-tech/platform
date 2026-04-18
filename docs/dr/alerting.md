# In-cluster alerting

`kube-prometheus-stack` (Prometheus + Alertmanager + node-exporter +
kube-state-metrics) running per cluster, no Grafana, no remote-write, no
SaaS. Alerts ship via webhook to a free destination (Discord channel /
email-to-webhook bridge) — the URL is per-cluster and SOPS-encrypted.

## Why no Grafana

This is **alerting only**. Operators look at logs and `kubectl` for
debugging; we don't run a dashboard tier on the homelab to keep the
resource budget small (Grafana adds ~512 MiB and another HelmRelease to
keep current).

## Why no remote-write

Same reason — no external dashboard tier. Critical alerts route directly
out of Alertmanager.

## What gets alerted

See `k8s/bases/infrastructure/controllers/kube-prometheus-stack/rules/critical.yaml`.

| Alert                       | Severity | Why                                       |
| --------------------------- | -------- | ----------------------------------------- |
| `NodeNotReady`              | critical | Single node loss; PDBs cover but you should still know |
| `NodeDiskFillingUp`         | warning  | >90% root fs                              |
| `PersistentVolumeFillingUp` | critical | >90% PVC                                  |
| `CertificateExpiringSoon`   | warning  | <14 d to expiry, cert-manager not renewing |
| `FluxKustomizationNotReady` | critical | Reconciliation broken >15 min             |
| `VeleroBackupFailed`        | critical | Any failure in last hour                  |
| `VeleroNoRecentBackup`      | critical | RPO breach -- no successful backup in 30h |
| `CNPGNoRecentBackup`        | critical | Same, for Postgres                        |
| `CNPGClusterDegraded`       | critical | Primary alone, no streaming replica       |

`defaultRules.create: false` is set on the chart so we don't drown in the
~200 generic chart-bundled alerts that aren't useful at homelab scale.

## Caveat: in-cluster Alertmanager won't fire if the whole cluster is down

This is the deliberate tradeoff for "no SaaS". Mitigations:

1. **Daily Velero schedule + Omni etcd backups still run independently.**
   On next recovery, you'll see the missed backup in R2.
2. **PR #7 CI restore drill** asserts the rules render and Alertmanager
   accepts them on every PR — so a regression in the alert spec is caught
   before merge.
3. If true off-cluster alerting becomes necessary later, the documented
   follow-up is to add Grafana Cloud free tier (10k metrics, ample for
   these alerts) and configure a remote-write target in
   `prometheus.prometheusSpec.remoteWrite`. No code restructure required.

## Per-environment webhook URL

Stored in `variables-cluster-secret.enc.yaml` as `alertmanager_webhook_url`,
substituted into the `alertmanager-webhook` Secret at apply time.

| Env   | Where to set                                  | Suggestion             |
| ----- | --------------------------------------------- | ---------------------- |
| local | `k8s/clusters/local/variables/variables-cluster-secret.enc.yaml` (already filled with a non-resolvable invalid URL — alerts fail to send, on purpose) | n/a |
| dev   | same path under `clusters/dev/`               | Discord #dev-alerts    |
| prod  | same path under `clusters/prod/`              | Discord #prod-alerts   |

To set:

```bash
sops --set '["stringData"]["alertmanager_webhook_url"] "<url>"' \
  k8s/clusters/<env>/variables/variables-cluster-secret.enc.yaml
```

### Discord webhook recipe

1. Server settings → Integrations → Webhooks → New Webhook → copy URL.
2. Append `/slack` to the URL — Discord accepts Slack-formatted payloads
   natively, and Alertmanager's `slack_configs` is a closer match. Or use
   a tiny shim (e.g. `alertmanager-discord`) — tracked as a possible
   follow-up but not required.
3. Drop the URL into the SOPS secret per the command above.

### Email-to-webhook bridge

Free options: Mailgun (5k/mo free), Resend (3k/mo free), or AWS SES via
its HTTPS API. Configure the same way — paste the webhook URL into the
encrypted secret.

## Local clusters

Identical install, with:

- Webhook URL pointed at `http://example.invalid/no-webhook-on-local`
  (deliberately fails). CI asserts this fail mode is acceptable — the
  alerts still fire inside Alertmanager, the webhook just can't reach
  anywhere. PR #7 verifies the alert wiring; the lack of an external
  destination is by design.

## Tuning resource footprint

Current chart values:

| Component      | Requests              | Limits        |
| -------------- | --------------------- | ------------- |
| Prometheus     | 100m CPU / 512 Mi     | — / 1 Gi      |
| Alertmanager   | 50m CPU / 64 Mi       | — / 128 Mi    |
| Operator       | 50m CPU / 128 Mi      | — / 256 Mi    |
| node-exporter  | (chart defaults)      | (chart defaults) |
| kube-state-metrics | (chart defaults)  | (chart defaults) |

Total ~1 GiB committed memory, well within 3× CX23 (24 GiB total). If
this becomes too heavy, the first thing to drop is `nodeExporter` and
the related node-level alerts.

## Related

- [DR runbook](./runbook.md) — what to do when an alert fires
- [Velero + CNPG](./velero-cnpg.md) — the systems whose health is being checked
- [HA primitives](../../README.md) — PDBs / topology spread (PRs #2a/#2b)
