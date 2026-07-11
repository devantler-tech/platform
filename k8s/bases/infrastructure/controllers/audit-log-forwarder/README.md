# Audit-log forwarder

A control-plane-only OpenTelemetry Collector DaemonSet that tails the
kube-apiserver audit log (`/var/log/audit/kube/audit.log`, written by
`talos/cluster/enable-audit-logging.yaml`) and ships it to Coroot's OTLP logs
endpoint, making audit events searchable in the Coroot UI alongside container
logs and traces.

- [OpenTelemetry Collector Helm chart](https://github.com/open-telemetry/opentelemetry-helm-charts/tree/main/charts/opentelemetry-collector)
- [Coroot OpenTelemetry log ingestion](https://docs.coroot.com/logs/opentelemetry/)

It lives in the `observability` namespace on purpose: that namespace already
carries the privileged Pod Security Standards label, the Kyverno
host-restriction/security-context excludes, and the Kubescape
`infrastructure-privileged` exception that the hostPath + run-as-root reader
needs, and the existing `allow-coroot` CiliumNetworkPolicy covers the
intra-namespace push to `coroot-coroot:8080`. The on-node audit file (30-day
rotation) remains the resilient primary; Coroot holds the searchable copy for
its `logsTTL` window.
