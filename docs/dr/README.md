# Disaster recovery & high availability

This directory holds the operator-facing DR documentation for the platform.
Start with [`runbook.md`](./runbook.md); the other documents go deeper on the
specific layers it references.

| Document                                       | Layer                |
| ---------------------------------------------- | -------------------- |
| [runbook.md](./runbook.md)                     | Procedure            |
| [crypto-custody.md](./crypto-custody.md)       | Cryptographic roots  |
| [openbao.md](./openbao.md)                     | Secret store         |
| [velero-cnpg.md](./velero-cnpg.md)             | Apps + PVs           |
| [alerting.md](./alerting.md)                   | Detection            |
| [restore-drill.md](./restore-drill.md)         | CI verification      |

Three further documents record one-off work rather than a standing procedure:

| Document                                                           | What it is                                                                   |
| ------------------------------------------------------------------ | ---------------------------------------------------------------------------- |
| [cnpg-storageclass-migration.md](./cnpg-storageclass-migration.md) | Runbook for moving a database to the `longhorn-wffc` storage class.          |
| [openbao-raft-ha-migration.md](./openbao-raft-ha-migration.md)     | Record of the move from one OpenBao server to three.                         |
| [spire-server-ha.md](./spire-server-ha.md)                         | A design that is not implemented; SPIRE is currently switched off.           |

These are operator documents. The commands in the first two change a running
cluster and need production access; the third is a design with nothing to run.
None of them is a check to run on a contribution.
