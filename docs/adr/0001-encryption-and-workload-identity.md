# ADR-0001: Network encryption and workload identity (Cilium on Talos)

**Status:** Accepted (egress strict mode pending via [#1804](https://github.com/devantler-tech/platform/pull/1804))
**Date:** 2026-06-05
**Deciders:** devantler-tech maintainers

## Context

The platform is a small (~7-node) self-hosted **Talos Linux + Hetzner Cloud**
cluster with **Cilium 1.19.4** as the CNI and **Kubernetes v1.35.5**, delivered
via Flux GitOps. Three threats drive the encryption/identity posture:

1. **Wire eavesdropping** — pod and node traffic crosses Hetzner's private
   network, a shared fabric we do not control end-to-end. Traffic must be
   encrypted in transit.
2. **Lateral movement / workload impersonation** — any pod can attempt to reach
   any other (subject to policy). We want workload-level segmentation *and*
   cryptographic proof of *which* workload is talking.
3. **Data at rest** — recycled cloud volumes / stolen disks must not yield data.

Two facts shape every option below:

- **Encryption and authentication are orthogonal axes.** Encryption gives
  confidentiality on the wire and authenticates the *node*; workload identity
  authenticates the *workload* (namespace/ServiceAccount). One never subsumes
  the other — Cilium's own guidance is to run an encryption layer *and* mutual
  authentication together.
- **The stack is prod-only.** The docker/local overlay is a single Talos
  container with no node-to-node wire, so it disables encryption and SPIRE
  ([providers/docker/.../cilium/patches/helm-release-patch.yaml](../../k8s/providers/docker/infrastructure/controllers/cilium/patches/helm-release-patch.yaml)).
  This means CI's Talos+Docker system test does **not** exercise the encryption
  path — prod-only changes here need live validation.

History worth recording: SPIRE mutual auth was **removed** once (#1776, as
"enforced by zero policies") and then **re-added and enforced** (#1781/#1788).
This ADR exists so that decision is not re-litigated a third time.

Non-functional constraints: we are **not** subject to FIPS/regulatory
compliance; the cluster is small (a single-writer SQLite `spire-server`); and we
rely heavily on **CiliumNetworkPolicy** (a default-deny baseline + 39 policies)
for segmentation — any option that breaks L4 policy enforcement is disqualifying.

## Decision

Adopt a layered, defence-in-depth composition. Each layer is independent and
individually swappable:

| Layer | Choice | Where |
|-------|--------|-------|
| Wire encryption | Cilium **WireGuard** + `nodeEncryption` | [bases/.../cilium/helm-release.yaml](../../k8s/bases/infrastructure/controllers/cilium/helm-release.yaml) |
| Fail-closed encryption | Cilium **egress strict mode** (pod CIDR `10.244.0.0/16`) | [providers/hetzner/.../cilium/patches/helm-release-patch.yaml](../../k8s/providers/hetzner/infrastructure/controllers/cilium/patches/helm-release-patch.yaml) |
| Workload identity | **SPIRE mutual auth** (`authentication.mode: required`) | [providers/hetzner/.../cilium/clusterwidenetworkpolicy.yaml](../../k8s/providers/hetzner/infrastructure/controllers/cilium/clusterwidenetworkpolicy.yaml) |
| Network authorization | **default-deny** + 39 CiliumNetworkPolicies | [bases/.../best-practices/add-default-deny.yaml](../../k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml) |
| Encryption at rest | Talos **LUKS2** (STATE + EPHEMERAL, nodeID-derived) | [talos/cluster/disk-encryption.yaml](../../talos/cluster/disk-encryption.yaml) |
| Host / runtime | Talos ingress firewall + Tetragon + Kubescape | `talos/`, `bases/.../controllers/{tetragon,kubescape}/` |

Explicitly **rejected**: Talos KubeSpan (kept off), Cilium IPsec, Cilium ztunnel
"native mTLS", and Kubernetes KEP-4317 Pod Certificates (see below).

## Options considered

### Encryption axis

| Option | Maturity | Coverage | NetworkPolicy compat | Operational cost | Verdict |
|--------|----------|----------|----------------------|------------------|---------|
| **Cilium WireGuard + nodeEncryption** | Stable | cross-node pod+node (TCP/UDP) | full | low (auto keys) | **Chosen** |
| Cilium IPsec | Stable | cross-node pod+node | full | higher (manual key rotation) | Rejected |
| Talos KubeSpan | Stable | node mesh | conflicts w/ Cilium datapath | low | Rejected (off) |
| Cilium ztunnel native mTLS | **Beta** | same+cross-node, **TCP only** | **breaks L4 policy** | adds per-node proxy | Rejected |

- **WireGuard** is the modern default: automatic per-node key management, strong
  performance, and `nodeEncryption: true` extends it to node/pod-to-node traffic.
- **IPsec**'s only decisive advantage is FIPS-validated AES-GCM. We are not
  regulated, and it costs manual key rotation — no reason to switch.
- **KubeSpan** is a Talos-layer WireGuard mesh meant for clusters spanning
  multiple networks with no private backbone. With Cilium it causes asymmetric
  routing ("Talos intercepts that traffic and routes it through the WireGuard
  mesh… broken pod-to-pod communication"), and it only carries **node-level**
  identity. On a single Hetzner private network with Cilium, CNI-native
  encryption is idiomatic; running both would be redundant double-encryption.
- **ztunnel native mTLS** (the [March 2026 Cilium blog](https://cilium.io/blog/2026/03/23/native-mtls-cilium/)) was attractive (same-node encryption, combined enc+auth) but disqualified on our stack: it **disables L4 CiliumNetworkPolicy for enrolled pods** ("L4 policies won't work except… port 15008"), requires **both endpoints enrolled** with no incremental rollout (host-network kube-system *cannot* enroll), is **TCP-only** (DNS/UDP unencrypted), still requires **SPIRE**, and adds a per-node proxy. It sacrifices a working, depended-on control (policy) for an immature one, with no identity simplification.

### Workload-identity axis

| Option | Maturity | Property | Cilium support | Verdict |
|--------|----------|----------|----------------|---------|
| **SPIRE mutual auth + NetworkPolicy** | Beta (mature) | cryptographic workload identity | native (only CA mode) | **Chosen** |
| NetworkPolicy only (drop SPIRE) | Stable | label-asserted identity | native | Rejected for best-security |
| KEP-4317 Pod Certificates | **Beta** | kubelet-issued certs | **not consumed by Cilium** | Rejected (watch) |

- **SPIRE mutual auth** adds cryptographic proof of workload identity (SPIFFE
  SVID) on top of label-based NetworkPolicy — the zero-trust layer. Its cost
  (single-writer `spire-server` in the data path; the [cilium#40533](https://github.com/cilium/cilium/issues/40533)
  bootstrap workaround) is accepted as the price of that posture.
- **Dropping SPIRE** is defensible *only* under a simplicity criterion (trusting
  NetworkPolicy alone). Under a best-security criterion it loses cryptographic
  identity, so it is rejected here. Note: WireGuard does **not** make SPIRE
  redundant — WireGuard authenticates nodes, SPIRE authenticates workloads.
- **KEP-4317 Pod Certificates** (Beta in k8s 1.35; we run v1.35.5) is a
  kubelet-issued cert *primitive* for app-level / pod-to-apiserver mTLS. Cilium
  does **not** consume it — SPIRE is "the only CA mode supported." Being on
  1.35.5 therefore does **not** let us drop SPIRE. Watch item only.

## Trade-off analysis

The central trade-off is **independent mature layers vs. a single combined
mechanism**. The temptation is to collapse encryption + identity into one path
(ztunnel) or to assume one axis subsumes the other ("WireGuard encrypts
pod-to-pod, so SPIRE is redundant"). Both are category errors: encryption ≠
authentication, and node identity ≠ workload identity.

We keep the axes **separate and mature**: WireGuard (stable) for the wire, SPIRE
mutual auth for identity, and CiliumNetworkPolicy for authorization — each
swappable without touching the others. This costs us same-node TCP encryption
(only ztunnel offers it today) and keeps SPIRE's operational burden, but it
preserves the L4 policy model we depend on and avoids betting the cluster's
segmentation on a Beta feature.

## Consequences

**Easier**
- A documented, stable posture; each layer reasoned about and replaced independently.
- Egress strict mode ([#1804](https://github.com/devantler-tech/platform/pull/1804)) closes the WireGuard fail-open window — pod traffic that can't yet be encrypted is dropped, not leaked.

**Harder**
- SPIRE's operational weight stays: single-writer `spire-server` sits in the data path; the cilium#40533 chown workaround remains until upstream fixes it.
- Strict mode is **fail-closed and prod-only**, so CI cannot exercise it; a wrong pod CIDR would drop prod pod traffic. Mitigated by validation on first reconcile.
- Same-node pod-to-pod TCP is unencrypted (it never leaves the host; not on any wire).

**Revisit when**
- Cilium ships a **Kubernetes-pod-certificate identity backend** → reconsider dropping SPIRE.
- ztunnel reaches **GA *and* preserves CiliumNetworkPolicy enforcement** → reconsider same-node encryption.
- **FIPS** becomes a requirement → reconsider IPsec.
- `spire-server` availability becomes a concern → move it to an external SQL datastore for real HA.

## Action items

1. [x] Implement Cilium egress encryption strict mode — [#1804](https://github.com/devantler-tech/platform/pull/1804).
2. [ ] Merge #1804; verify pod-to-pod connectivity + `cilium-agent` logs on first prod reconcile.
3. [ ] (Optional follow-up) Enable **ingress** strict mode once egress is confirmed healthy.
4. [ ] Watch: Cilium ↔ KEP-4317 identity backend; ztunnel ↔ NetworkPolicy coexistence.
