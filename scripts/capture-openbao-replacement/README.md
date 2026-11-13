# capture-openbao-replacement

Captures, from production, the two observations the single-server OpenBao replacement procedure
(#4970) judges before it replaces anything. It changes nothing and decides nothing: the
[health judge](../judge-openbao-health/README.md) and the
[quorum judge](../judge-openbao-quorum/README.md) decide from what it writes.

```sh
capture-openbao-replacement \
  --expect-servers openbao-0,openbao-1,openbao-2 \
  --out <private-dir>
```

The protected [`Replace OpenBao Server`](../../.github/workflows/replace-openbao-server.yaml)
workflow runs it, then both judges for the server it was asked about, before it replaces anything and
again after each replacement until both judges return `PROVEN`.
The expected servers and release come from the reviewed render, never from the cluster.

| Exit | Line | Meaning |
| --- | --- | --- |
| 0 | `OPENBAO_REPLACEMENT_EVIDENCE=CAPTURED` | `health.json` and `quorum.json` are in `--out`, readable only by the owner. |
| 2 | `OPENBAO_REPLACEMENT_EVIDENCE=UNVERIFIED stage=<stage>` | A read failed, so nothing was written. Invalid arguments also exit 2. |

The stage is one of `pod-identity`, `server-health`, `identity-token`, `observer-login`,
`raft-configuration`, `token-revoke` or `private-capture`. There is no HOLD: a sealed server or
a non-200 Raft read is recorded as it was observed, and the judges turn it into HOLD or UNKNOWN.

## What it reads

- **Health:** each expected server through a port-forward to that pod alone, so a body is always
  filed under the server that produced it. Each read is bound to the pod UID seen before the
  first read; a UID that differs afterwards fails the capture, so a pod replaced mid-capture
  cannot lend its evidence to its successor.
- **Raft configuration:** one authenticated read through `service/openbao-active`. The command
  mints a ten-minute token for the workload-less `openbao-replacement-observer` service account,
  logs in to the Kubernetes-auth role of the same name, reads once and revokes the OpenBao token.
  A token it cannot revoke fails the capture. The role carries no default policy, its token lives
  five minutes at most, and its only policy reads `sys/storage/raft/configuration` and revokes
  itself (`k8s/bases/infrastructure/vault-config/job.yaml`).

It prints its one status line and never a token, address or response body.

Run the regressions with `go test ./scripts/capture-openbao-replacement`. They stand in for the
cluster and the servers, run the real judges on the captures, and pin the workflow's protection
and the kubectl verbs the command may use.
