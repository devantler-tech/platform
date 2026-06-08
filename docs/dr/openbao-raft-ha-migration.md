# OpenBao single-node → 3-node Raft HA migration

> **Status: runbook / not yet executed.** This is the one HA item that is **not**
> a GitOps auto-apply. OpenBao currently runs `standalone` mode with **file**
> storage; flipping the HelmRelease to Raft without a data migration brings up
> empty raft pods and **wipes every secret cluster-wide** (all ExternalSecrets,
> PushSecrets, the CNPG/FleetDM DB-engine roles). Execute the steps below
> deliberately; do not merge the values change ahead of the migration.

## Why this can't be a values flip

| | Current | Target |
|---|---|---|
| Mode | `server.standalone.enabled: true` | `server.ha.enabled` + `ha.raft.enabled` |
| Storage | `storage "file"` (`/openbao/data`) | `storage "raft"` (Integrated Storage) |
| Replicas | 1 (`openbao_replicas: "1"`) | 3 (Raft quorum minimum) |
| Seal | Shamir 1-of-1, key in `openbao-unseal` Secret, `postStart` auto-unseal | unchanged (or transit auto-unseal — see hardening) |

Raft is a different storage backend. New raft pods initialize an **empty** store;
the existing file-storage PVC data is orphaned. Data must be carried over with
either `bao operator migrate` (server offline) or a fresh init + snapshot
restore. The Shamir unseal key and root token **survive both paths** (migrate
preserves the seal; a snapshot is taken post-unseal).

## Target HelmRelease values (apply only as part of the cutover)

Replace the `server.standalone` block in
`k8s/bases/infrastructure/controllers/openbao/helm-release.yaml` with:

```yaml
server:
  # standalone and ha are mutually exclusive — standalone MUST be disabled.
  standalone:
    enabled: false
  ha:
    enabled: true
    replicas: ${openbao_replicas:=1}   # set openbao_replicas: "3" in prod
    raft:
      enabled: true
      setNodeId: true                  # BAO_RAFT_NODE_ID = pod name (openbao-0/1/2)
      config: |
        ui = true
        disable_mlock = true           # keep — matches the PSS-baseline constraint

        listener "tcp" {
          tls_disable     = 1
          address         = "[::]:8200"
          cluster_address = "[::]:8201"
        }

        storage "raft" {
          path = "/openbao/data"
          # Auto-join: each follower discovers the cluster via the headless
          # service so no manual `bao operator raft join` is needed.
          retry_join { leader_api_addr = "http://openbao-0.openbao-internal:8200" }
          retry_join { leader_api_addr = "http://openbao-1.openbao-internal:8200" }
          retry_join { leader_api_addr = "http://openbao-2.openbao-internal:8200" }
        }

        service_registration "kubernetes" {}

        # Re-declare the file audit device (currently in standalone.config).
        audit "file" {
          type    = "file"
          options = { file_path = "/openbao/audit/audit.log" }
        }
```

Keep the existing `readinessProbe.path`, the `unseal-keys` volume + `postStart`
auto-unseal hook (it covers all 3 pods), `dataStorage` (now one RWO PVC per
replica: `data-openbao-0/1/2`), and `auditStorage`. The hetzner overlay's
`storageClass: hcloud` patch still applies. Set `openbao_replicas: "3"` in
`k8s/clusters/prod/bootstrap/variables-cluster-config-map.yaml` (and remove
`openbao` from the `validate-replica-floor` namespace exemptions) **as the last
step**, once the cluster is healthy.

## Network prerequisite — open the Raft cluster port (8201)

Single-node only needed `:8200`. Raft peers talk on **`:8201`**. Before the
cutover:

- **Cilium**: allow pod-to-pod `:8201` among openbao pods in
  `k8s/bases/apps/.../networkpolicy` (or the openbao netpol) — ingress on 8201
  from the openbao pod selector.
- **Talos firewall** (block mode): if openbao pods can land on different nodes,
  ensure the node-to-node allowlist covers `:8201` (cf. the Cilium mutual-auth
  `:4250` precedent — `talos/.../NetworkRuleConfig`).

This netpol/firewall prep is **safe to land ahead of time** (it only permits
traffic that doesn't exist yet at 1 node) and is the only part of this migration
that can go in via a normal PR.

## Migration — recommended path: snapshot + fresh re-init + restore

This is cleaner on Kubernetes than `bao operator migrate` (which wants a stopped
process with both filesystems mounted) and it exercises the DR restore path.

1. **Snapshot the current vault (rollback point).** With the single node unsealed:
   ```sh
   kubectl -n openbao exec openbao-0 -- bao operator raft snapshot save /tmp/pre-raft.snap 2>/dev/null \
     || kubectl -n openbao exec openbao-0 -- sh -c 'bao read -format=json sys/storage/raft/snapshot' # file-mode: use the vault-snapshot CronJob output instead
   ```
   For file storage there is no raft snapshot; rely on the existing
   `vault-backup` CronJob's latest snapshot, **and** `velero backup` the openbao
   namespace + PVCs. Verify the backup exists before proceeding.
2. **Record the unseal key + root token** (already in the `openbao-unseal`
   Secret; confirm you can read it out-of-band — the SOPS-encrypted copy is the
   source of truth).
3. **Apply the Raft values** (above) + `openbao_replicas: "3"` + the netpol. Flux
   brings up `openbao-0/1/2` with empty raft stores, sealed.
4. **Initialise exactly once, on openbao-0:**
   ```sh
   kubectl -n openbao exec -ti openbao-0 -- bao operator init -key-shares=1 -key-threshold=1
   # save the NEW unseal key + root token; update the openbao-unseal Secret (SOPS)
   kubectl -n openbao exec -ti openbao-0 -- bao operator unseal <new-unseal-key>
   ```
   With `retry_join`, openbao-1 and openbao-2 auto-join openbao-0; then unseal
   each (the `postStart` hook does this once the Secret carries the new key, or
   unseal manually):
   ```sh
   kubectl -n openbao exec -ti openbao-1 -- bao operator unseal <new-unseal-key>
   kubectl -n openbao exec -ti openbao-2 -- bao operator unseal <new-unseal-key>
   kubectl -n openbao exec -ti openbao-0 -- bao operator raft list-peers   # expect 3 voters
   ```
5. **Restore the data** into the fresh raft cluster:
   ```sh
   kubectl -n openbao exec -ti openbao-0 -- bao operator raft snapshot restore /tmp/pre-raft.snap
   ```
   (If migrating from file storage where no raft snapshot exists, use
   `bao operator migrate` instead — see the alternative path below — or
   re-seed via the `vault-config`/`vault-seed` Jobs if the secret material is
   reproducible. Most KV/dynamic-secret config here is re-seedable from
   `vault-config`, but **app data and rotated DB passwords are not** — prefer a
   real snapshot restore.)
6. **Verify**: `bao status` on all 3 (unsealed, one leader), `bao secrets list`,
   and confirm a sample ExternalSecret re-syncs.
7. **Land the manifest** (values + `openbao_replicas: "3"` + drop the openbao
   namespace from `validate-replica-floor`) so the state is captured in Git.

### Alternative: `bao operator migrate` (file → raft, preserves data + seal)

Take OpenBao **offline**, then run `bao operator migrate` with
`storage_source "file"` + `storage_destination "raft"` + `cluster_addr`. It
copies at the storage layer **without decrypting**, so the existing Shamir key +
root token keep working and all KV/auth/policies/DB-engine config carry over.
After migrate the raft cluster has a single node; bring up replicas 1–2, let them
`retry_join`, and unseal. Heavier on Kubernetes (needs a stopped process with
both backends mounted), so the snapshot-restore path above is preferred.

## Risks (data-loss scenarios) and mitigations

| Risk | Mitigation |
|---|---|
| **Flip values without migrating → empty cluster.** | This runbook: snapshot/backup first, init once, restore. Never merge the values flip standalone. |
| **Double-init split-brain** (init races on >1 pod → two 1-node clusters). | `bao operator init` exactly once on openbao-0; let others `retry_join`. |
| **Unseal-before-join** (a follower unsealed with mismatched keys before joining corrupts membership). | Join first (`retry_join` handles it), then unseal followers with the **new** key. |
| **Even-node quorum loss.** | Always odd (3). 3 voters tolerate 1 failure; the chart's default PDB `maxUnavailable: 1` is correct. |
| **8201 blocked** → joins wedge. | Open Cilium netpol + Talos firewall for `:8201` (above) before cutover. |
| **Audit fail-closed × 3.** | The file audit device fails closed when its PVC fills; now there are 3 `audit-openbao-*` PVCs — monitor all three `kubelet_volume_stats_available_bytes`. |
| **No pre-cutover backup → unrecoverable.** | Confirm a fresh `vault-backup` snapshot **and** a Velero backup of the openbao namespace + PVCs before step 3. |

## Hardening (follow-up, not required for HA)

A single 1-of-1 Shamir key in a Kubernetes Secret is a single point of
compromise. HA is a good moment to move to **transit auto-unseal** (a small
separate OpenBao/Vault via a `seal "transit" {}` stanza) or cloud KMS, and a
higher key-share threshold — this also removes the `postStart` unseal hook
dependency. Track separately.
