# Crossplane synchronization alert recovery

The managed-resource collector and scheduled coverage checker are configured in Git. Their private readback must match the reviewed configuration, permissions and loaded checksum; a ready Pod alone is insufficient.

After a monitoring repair deploys, run **Read Crossplane Alert Recovery** from the reviewed main revision, confirming `read-crossplane-alert-recovery`. Only a first-attempt main dispatch in this repository is admitted. The workflow serializes with production deployment, builds its fixed observer before restoring existing CI credentials, and uses the checksum-verified Kubernetes client. It changes no cluster object or alert.

The observer reads only the collector ConfigMap and Deployment, the existing coverage CronJob, and each of the two Alertmanager peers. It requires the complete reviewed ConfigMap, the loaded deployment checksum, a fully ready observed rollout, and a successful checker run within seven minutes and after that rollout. Both peer lists must be complete and valid. Any retained Crossplane alert, including a silenced or inhibited alert, prevents recovery. Unreadable, malformed, stale or oversized evidence is UNKNOWN. Raw responses and native errors are never logged or uploaded.

A successful observation prints `CROSSPLANE_RECOVERY=RECOVERED`. This establishes the named monitoring repair's current recovery only. It does not clear unrelated alerts, prove portfolio health, or establish ongoing alert delivery. Preserve the dispatch revision and run alongside the repair's acceptance record, and allow the existing alert expiration lifecycle to operate normally.

Offline regression: `go test -race ./scripts/observe-crossplane-recovery`. The tests use controlled API responses and never contact a cluster.
