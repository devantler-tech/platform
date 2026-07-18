# SPIRE server HA — external datastore + 2 replicas (design)

> **Status: DESIGN ONLY — no manifest change ships in this PR.** Coroot flags
> `kube-system:StatefulSet:spire-server` as *"single instance — not resilient to
> node failure."* That signal is **real**, but the fix is **not** a values flip.
> spire-server is installed by the **Cilium** chart (not a standalone SPIRE
> chart), and naïve HA would (a) split-brain on the built-in SQLite datastore
> and (b) add a datastore dependency to identity infrastructure. Earlier versions
> of this design assumed a blanket `require-mutual-auth` Cilium policy; that
> policy was removed because Cilium ingress rules are allow rules and a catch-all
> authentication rule weakens workload allow-lists. This doc remains the runbook
> for doing SPIRE HA correctly when the prerequisites are met; until then the **deliberate
> single-node posture stays**, and the replica-floor policy keeps spire-server
> exempt on purpose.

## TL;DR — why this isn't shipped as code today

Three independent blockers, each fatal to a blind change:

1. **The Cilium chart does not expose an external SQL datastore for spire-server.**
   Under `authentication.mutual.spire.install.server` the chart (cilium 1.19.4,
   the pinned version) exposes only the SQLite `dataStorage` PVC
   (`dataStorage.{enabled,size,accessMode,storageClass}`) — **no** `dataStore` /
   SQL-plugin / PostgreSQL keys, **no** `replicaCount`, and **no** hook to inject
   a custom `server.conf`. There is no in-chart path to point spire-server at
   PostgreSQL or to run >1 replica. HA therefore requires *changing how SPIRE is
   installed*, not changing values (see §3).
2. **Layering inversion.** spire-server is created by the **Cilium HelmRelease in
   the `infrastructure-controllers` Flux layer**. Every CloudNativePG **Cluster**
   in this repo (`umami-db`, the wedding-app DB, …) lives in the **`apps`** layer
   — *two* layers downstream
   (`infrastructure-controllers` → `infrastructure` → `apps`, by `dependsOn`).
   A datastore the identity controller depends on must come up **before** it, not
   two layers after. CNPG-for-SPIRE would have to be a brand-new
   `infrastructure-controllers`-tier Cluster, not a mirror of the apps-tier DBs.
3. **Identity/datastore coupling.** A CNPG datastore's data path is ordinary
   pod-to-pod traffic (`cnpg-system` operator → instance pods :5432). If Cilium
   authentication is reintroduced narrowly in the future, SPIRE must still be
   able to reach its datastore during bootstrap. Postgres is a busier,
   multi-pod, multi-node dependency than a single attached block device, so it
   must be staged deliberately.

**Verdict:** correct HA is a deliberate, staged migration of *identity infra* —
chart-install change + a dedicated bootstrap-tier datastore + a one-time SPIFFE
datastore cutover. Encoding only part of that (e.g. `replicas: 2` on SQLite, or a
CNPG Cluster in the wrong layer) would *create* an outage, not prevent one. So
this is an ADR, mirroring `docs/dr/openbao-raft-ha-migration.md` for the same
reason (critical security infra, disruptive cutover, data that must be carried
over, not blind-flipped).

## Current state (what Coroot is seeing)

| | Today |
|---|---|
| Install | Cilium chart sub-component (`authentication.mutual.spire`), `kube-system` |
| Replicas | **1** (StatefulSet; chart exposes no `replicaCount`) |
| Datastore | built-in **SQLite** on a single RWO PVC |
| PVC storage (prod) | **hcloud-csi** `hcloud` 10Gi (NOT longhorn — deadlock break, see overlay patch) |
| Data-path role | issues SVIDs for Cilium/SPIRE authentication consumers |
| Failure tolerance | survives **brief** restarts (cilium-agent auth cache + cilium-operator re-sync from CiliumIdentities ride a short gap); a node-loss outage lasts until the pod reschedules + its hcloud volume re-attaches |
| Replica-floor policy | spire-server **exempted on purpose** (`validate-replica-floor.yaml`) — "HA needs a shared external datastore, not just replicas" |

What single-replica actually costs here: SVID **issuance** (new identity pairs,
SVID rotation) is unavailable while spire-server is down. *Established* flows keep
working through cilium-agent's auth cache, and cilium-operator re-syncs entries
from CiliumIdentities, so a fast reschedule is tolerable — but a node failure with
slow volume re-attach is a real, if bounded, availability gap. That is the gap HA
closes.

## Why HA needs BOTH replicas AND a shared external datastore

SPIRE server HA is **not** "bump replicas." Each replica is a writer to the
datastore (registration entries, attested nodes, issued-SVID journal). The
built-in SQLite store is a single file on one RWO PVC:

- **>1 replica on SQLite = split-brain / corruption.** Two servers cannot share
  one RWO PVC (it mounts on one node), and even if they could, SQLite is not a
  shared multi-writer store. So HA *requires* swapping SQLite for a shared SQL
  datastore (SPIRE supports PostgreSQL/MySQL via its `sql` DataStore plugin).
- Only **with** a shared SQL datastore do ≥2 stateless spire-server replicas make
  sense, fronted by the existing `spire-server` Service, with a drain-safe PDB and
  topology spread.

Both halves are mandatory; either alone is wrong (replicas-only → split-brain;
datastore-only → still a SPOF).

## Target architecture

```
infrastructure-controllers layer (must be ready before SPIRE serves):
  cnpg operator (already here)  ──►  NEW CNPG Cluster: spire-db (cnpg-system or kube-system-adjacent infra ns)
                                         │  3 instances, longhorn? NO — see "datastore storage" below
                                         ▼
  spire-server (2 replicas, stateless)  ──sql plugin──►  spire-db  (PostgreSQL, shared)
        ▲                                                     ▲
        │ Cilium/SPIRE identity consumers                         │ creds via ExternalSecret/OpenBao (repo pattern)
        └─ MUST be able to reach spire-db WITHOUT a working SVID (bootstrap carve-out)
```

### The hard part: breaking the SPIRE→Postgres→SPIRE cycle

The datastore must be reachable by spire-server **before** SPIRE is issuing
SVIDs, or it deadlocks. Options, hardest constraint first:

- **Datastore storage must NOT be Longhorn.** Same reason the SQLite PVC isn't:
  Longhorn's control plane is itself pod-to-pod traffic. A CNPG `spire-db` on
  Longhorn re-arms the original deadlock one layer over. Use **hcloud-csi**
  (`hcloud` StorageClass) for `spire-db`'s PVCs — attaches via the Hetzner API,
  not pod-to-pod, and survives node death. (Trade-off: CNPG HA wants pod
  anti-affinity across the 3 storage workers; hcloud volumes are
  `WaitForFirstConsumer` per-AZ — confirm topology lands one instance per node
  without a Longhorn replica set. A 1-instance `spire-db` on hcloud + frequent
  base backups may be the pragmatic first cut, accepting that the DB itself is
  then the SPOF the replicas removed from the server tier — see "Open questions".)
- **Do not add a broad mTLS carve-out or catch-all authentication rule.** If
  Cilium authentication is reintroduced for SPIRE↔Postgres in the future, it must
  be scoped to the exact datastore traffic. A blanket `fromEndpoints: [{}]`
  authentication policy is an allow rule and weakens workload isolation.
- **Talos node firewall** already allows the SPIRE mesh-auth port 4250
  node-to-node (`talos/workers/allow-cilium-mutual-auth-ingress.yaml`, `talos/control-planes/allow-internal-node-ingress.yaml`). Postgres
  :5432 between nodes is intra-cluster pod traffic over the CNI, not a host port,
  so no Talos firewall change is expected — **verify** spire-db instances and
  spire-server can co-locate or cross nodes without a host-firewall drop.

### Datastore connection + credentials (repo pattern)

- **Cluster:** new `spire-db` CNPG `Cluster` mirroring `umami/cluster.yaml`
  *structurally* (managed role, superuser-for-OpenBao, R2 Barman backups via the
  plugin) but placed in an **infrastructure-controllers**-tier directory and on
  **hcloud** storage, not longhorn. Bootstrap an empty `spire` DB owned by a
  `spire` role.
- **Credentials:** follow the established secret flow — CNPG publishes
  `spire-db-app` (host/port/user/password/dbname/uri); OpenBao Database secrets
  engine rotates the `spire` role (superuser pushed to OpenBao via a PushSecret as
  umami does); spire-server consumes an OpenBao-synced ExternalSecret. **Caveat:**
  OpenBao/External-Secrets are themselves in the `infrastructure` layer and part of identity bootstrap — for the *bootstrap* connection SPIRE may need the raw CNPG
  `spire-db-app` secret (same layer) rather than the OpenBao-rotated one, with
  rotation layered on only after steady state. Resolve in the spike (Open
  questions).
- **SPIRE SQL plugin config:** spire-server needs a `DataStore "sql"` plugin
  stanza (`database_type = "postgres"`, `connection_string = …`) injected into
  `server.conf`. **The Cilium chart does not support this** (it renders its own
  server config and exposes no override). This is the crux of §3 below.

## §3 — The install must change (chart limitation)

Because the Cilium chart exposes neither a SQL datastore nor a `replicaCount` nor
a `server.conf` override for spire-server, one of these is required — in
increasing order of blast radius:

1. **Disable Cilium's bundled SPIRE install and deploy SPIRE from its own chart**
   (`authentication.mutual.spire.install.enabled: false`, keep
   `authentication.mutual.spire.enabled: true` so Cilium still *uses* it, then run
   the upstream `spiffe/spire` / `spire-server` chart configured for HA + the SQL
   datastore). This is the SPIRE-supported HA path and the cleanest long-term, but
   it means **owning the full SPIRE deployment** (server, agent DaemonSet, the
   Cilium registration entries / `cilium-init` bootstrap the bundled chart wires
   up today — see the `podSecurityContext` 1000:1000 fix and the `cilium-init`
   ptrace note in `helm-release.yaml`). Substantial, and it must re-create the
   delegated-identity wiring cilium-agent depends on.
2. **Upstream the gap** — open a cilium issue/PR to expose
   `server.dataStore` (SQL plugin) + `server.replicaCount` in the bundled SPIRE
   chart, then adopt it on the next bump. Lowest blast radius for *this* repo, but
   not in our control / not immediate.
3. **Carry a patched server config** (e.g. a Kustomize patch over the rendered
   spire-server ConfigMap/StatefulSet to inject the SQL plugin + scale replicas).
   Fragile: fights the HelmRelease, breaks on every chart bump, and the drift-
   detection component would fight it. **Not recommended.**

Recommended sequence: **(a)** ship the additive prerequisites now (the
spire-db↔spire-server mTLS carve-out policy; optionally provision an *unused*
`spire-db` CNPG Cluster in the infra-controllers tier on hcloud so the datastore
exists and is backed up), then **(b)** do option 1 (standalone SPIRE chart) as a
dedicated, separately-reviewed migration with the cutover runbook below, or wait
out option 2 if upstream is responsive.

## Migration runbook (perform as one staged cutover — never a blind merge)

Mirrors the openbao raft migration discipline: back up, change install, carry the
data, verify, then capture in Git.

0. **Prereqs landed & verified (separate, additive PRs):**
   - exact, narrow network policy for spire-server↔spire-db `:5432` (and
     cnpg-operator↔spire-db), verified with a test pod before cutover.
   - `spire-db` CNPG Cluster live in the infra-controllers tier on **hcloud**
     storage, empty `spire` DB, R2 backups confirmed, creds secret published.
   - Decide bootstrap-cred source (raw CNPG secret vs OpenBao) — §"credentials".
1. **Back up current SPIFFE state (rollback point).** The registration entries are
   re-derivable (cilium-operator re-syncs from CiliumIdentities), but back up the
   live SQLite datastore PVC (Velero the `kube-system` spire-server PVC) and record
   the trust-domain/CA state before touching anything.
2. **Switch the install** (option 1): set
   `authentication.mutual.spire.install.enabled: false` and deploy the standalone
   SPIRE server chart with `replicaCount: 2`, the `DataStore "sql"` plugin →
   `spire-db`, the existing trust domain, a drain-safe PDB
   (`maxUnavailable: 1`, `minAvailable: null` — the repo pattern), and topology
   spread across nodes. Keep the spire-agent DaemonSet + the cilium delegated-
   identity socket wiring intact.
3. **Re-bootstrap identities.** With the empty SQL datastore, let cilium-operator
   re-create the Cilium registration entries (the same re-sync that covers a
   restart). Confirm `cilium-init`/operator seed the entries (watch for the
   "Waiting for spire-server to start…" hang the 1000:1000 `podSecurityContext`
   fix addresses — carry that fix forward).
4. **Verify** before declaring done:
   - both spire-server replicas Ready, each connected to `spire-db`;
   - `spire-server` Service endpoints = 2;
   - SVIDs issuing (no cluster-wide `no identity issued` storm; sample Cilium/SPIRE identity
     consumer works after cache expiry);
   - kill one spire-server pod → identities still issue (the actual HA assertion);
   - `spire-db` failover (kill primary) → spire-server reconnects.
5. **Land the manifests** (install change + spire-db + carve-out + drop
   spire-server from the `validate-replica-floor` exemption) so Git captures the
   HA state.

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| **Replicas on SQLite → split-brain/corruption.** | Never. HA requires the shared SQL datastore first; replicas only after the datastore cutover. |
| **SPIRE↔Postgres bootstrap coupling.** | Exact spire-server↔spire-db `:5432` policy, landed & verified **before** the cutover; avoid broad catch-all authentication policies; **spire-db on hcloud, not longhorn.** |
| **Layering inversion** (CNPG Cluster downstream of SPIRE). | Place `spire-db` in the **infra-controllers** tier, not `apps`; it must be ready before spire-server serves. |
| **Chart can't express SQL datastore / replicas / custom server.conf.** | Change the *install* (standalone SPIRE chart, `install.enabled: false`) or upstream the chart keys — do **not** hand-patch the rendered config (drift-detection + chart-bump fragility). |
| **Bootstrap credential chicken-and-egg** (OpenBao/ESO are later-layer dependencies). | Use the same-layer raw CNPG `spire-db-app` secret for the bootstrap connection; layer OpenBao rotation on only after steady state. |
| **`spire-db` becomes the new SPOF** (if run 1-instance on hcloud). | Prefer 3-instance CNPG if hcloud topology allows one-per-node; else accept a 1-instance DB with frequent R2 base backups + fast restore, and document that the server tier is HA even if the DB is the residual SPOF — still strictly better than today. |
| **Losing the bundled-chart wiring** (cilium-init entry seeding, delegated-identity socket, 1000:1000 ptrace fix). | The standalone deployment must re-create all of it; the existing `helm-release.yaml` comments are the spec. Verify identity issuance end-to-end in staging-equivalent before prod. |
| **No pre-cutover backup → unrecoverable trust state.** | Velero the spire-server PVC + record CA/trust-domain before step 2; entries are re-derivable but don't rely on it blindly. |

## Open questions (resolve in a spike before committing to a cutover date)

1. Does `spire-db` run 3-instance CNPG on hcloud-csi without a topology mismatch
   (one instance per storage worker, no Longhorn), or is a 1-instance + backups
   first cut the pragmatic call?
2. Standalone SPIRE chart vs. waiting on an upstream cilium chart enhancement —
   which lands sooner with less risk? (File the upstream issue regardless.)
3. Exact bootstrap credential path that doesn't route through OpenBao/ESO during
   the pre-identity window.
4. Is the residual availability gain worth the operational surface? Today's
   single-node SPIRE already rides brief restarts via the auth cache + operator
   re-sync; quantify the real-world outage window a node loss causes before
   committing to owning a full standalone SPIRE + a dedicated Postgres.

## Recommendation

Ship the **additive, safe** prerequisites first (the exact spire-server↔spire-db
network policy; optionally a backed-up `spire-db` CNPG Cluster in the infra-controllers
tier on hcloud) as small independent PRs, and **file the upstream cilium chart
enhancement** for SQL-datastore + replicas. Treat the full HA cutover (option 1
above) as a separately-reviewed migration executed with this runbook — **not** a
blind values flip. Until then, the single-node posture is the correct, documented
trade-off, and the `validate-replica-floor` exemption stays.
