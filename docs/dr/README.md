# Disaster recovery & high availability

This directory holds the operator-facing DR documentation for the platform.
Start with [`runbook.md`](./runbook.md); the other documents go deeper on the
specific layers it references.

| Document                                 | Layer            |
| ---------------------------------------- | ---------------- |
| [runbook.md](./runbook.md)               | Procedure        |
| [omni-etcd-backups.md](./omni-etcd-backups.md) | Control plane (PR #3) |
| [velero-cnpg.md](./velero-cnpg.md)       | Apps + PVs (PR #4) |
| [alerting.md](./alerting.md)             | Detection (PR #6) |
