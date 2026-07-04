# Requiring VPN in front of critical services (design)

> Status: **design / not yet implemented.** The WireGuard *listener* is
> [platform#2462](https://github.com/devantler-tech/platform/pull/2462); the
> UniFi *client* side is [unifi#9](https://github.com/devantler-tech/unifi/pull/9).
> This doc picks the enforcement approach **before** any access-control manifests
> touch prod, because the clean options are blocked by the live infra and getting
> it wrong locks admin access out.

## Goal

The admin UIs — **Coroot** (`observability`), **Hubble**, **OpenCost**,
**Longhorn**, **OpenBao** (`vault`), **Headlamp**, **KSail** — reachable **only**
through the WireGuard tunnel. Everything that must stay public stays public:
**Dex** and **oauth2-proxy** (the OIDC flow), and the normal public apps.

## Architecture recap

Talos control planes run a WireGuard **server** (`wg0` = `10.200.0.1/24`,
`51820/udp`, subnet `10.200.0.0/24`). The home UniFi gateway is a dial-in
**client** (`10.200.0.2/32`); it pushes routes `10.200.0.0/24`, `10.0.0.0/16`
(nodes), `10.244.0.0/16` (pods) through the tunnel. No route overlap: nodes
`10.0.0.0/16`, pods `10.244.0.0/16`, services `10.96.0.0/16`.

## Current state (live-verified on prod, Cilium v1.36.2)

| Fact | Value | Source |
| --- | --- | --- |
| Gateway | single Cilium `Gateway platform` (kube-system), HTTPS:443, wildcard `gateway-tls` | `k8s/bases/infrastructure/gateway/` |
| Admin routes | all 7 HTTPRoutes bind that Gateway, `allowedRoutes.from: All` | per-controller `http-route.yaml` |
| LB | `cilium-gateway-platform` Service `type=LoadBalancer` → **Hetzner Cloud LB** | `gateway-patch.yaml` (hcloud annotations) |
| LB IPs | public `49.12.20.241` (+ IPv6) **and private `10.0.1.7`**; ports 80/443 (nodePorts 32269/30755); `externalTrafficPolicy: Cluster` | live `svc` status |
| Public DNS | admin hostnames → **Cloudflare** (`188.114.96/97.1`) → proxied to the LB origin | `dig` |
| Cilium | `kube-proxy-replacement=true`, `routing-mode=tunnel/vxlan`, `enable-ipv4-masquerade=true`, LB-IPAM enabled **but no `CiliumLoadBalancerIPPool`**, **`devices = enp7s0 eth1`** | live `cilium-config` |
| Control planes | `10.0.1.1 / 10.0.1.2 / 10.0.1.3` (private) + public IPs | live `nodes` |
| Auth today | oauth2-proxy+Dex gates Coroot/Hubble/OpenCost/Longhorn; OpenBao/Headlamp/KSail have their own app auth | HTTPRoutes → `oauth2-proxy` vs direct |
| Existing net restriction | **none** — no `fromCIDR`/`toCIDR`, no internal gateway, no source firewall on 443 | grep |

## Why the two "obvious" approaches are blocked

1. **Internal VIP `10.200.0.10` announced to the tunnel** — there is **no Cilium
   LB-IPAM pool** and the gateway rides a **public Hetzner LB**, not an
   L2-announced node IP. L2 announcement (ARP) cannot cross the **L3** WireGuard
   tunnel, so you cannot make a client on `10.200.0.0/24` resolve a `10.200.0.10`
   VIP via ARP. A VIP would need explicit routing/binding, not announcement.

2. **`fromCIDR: 10.200.0.0/24` CiliumNetworkPolicy on the backends** —
   `enable-ipv4-masquerade=true`, so tunnel-sourced traffic is **SNAT'd to a node
   IP** before it reaches a pod. The policy would see source `10.0.0.0/16`, not the
   tunnel CIDR. Access control by **source-filtering is out**; it must be by
   **reachability** — bind the service to an address only the tunnel can route to.

Two more constraints shape every option:

- **Cilium is device-pinned to `enp7s0 eth1`, not `wg0`.** Traffic arriving on the
  tunnel is **not** in Cilium's BPF service/LB/externalIPs/nodePort datapath. So
  "give the gateway a tunnel externalIP" or "hit a nodePort over the tunnel" is
  **not** DNAT'd by Cilium unless `wg0` is added to `devices` — a core-datapath
  change that re-attaches BPF **cluster-wide** (high blast radius).
- **Public exposure is two layers:** Cloudflare DNS **and** the Hetzner LB. For an
  admin hostname to be truly VPN-only, **both** must stop serving it (while Dex
  stays public).

## Viable approaches

### A — Reuse the gateway's private IP (`10.0.1.7`) + CP tunnel forwarding
- Admin DNS (UniFi split-horizon, already in unifi#9) → **`10.0.1.7`** (in the
  tunnel's `10.0.0.0/16` route). The CP node forwards the tunnel packet out `eth1`
  → Hetzner LB (private) → envoy → backend.
- **Needs:** `net.ipv4.ip_forward=1` + an nftables masquerade rule for `wg0`↔`eth1`
  on the control planes (a Talos machine-config addition), and the return path to
  hold under `externalTrafficPolicy: Cluster`.
- **VPN-only:** remove the admin hostnames from **Cloudflare/public DNS**; keep Dex
  public. Optionally also drop admin routes from the public listener.
- **Pro:** reuses the gateway + LB; no Cilium `devices` change. **Con:** a hairpin
  (tunnel→CP→LB→node→envoy); forwarding+masquerade must be exactly right; still
  depends on closing public DNS.

### B — Host-network reverse proxy on the CPs, bound to `wg0:443` *(recommended)*
- A control-plane-only `hostNetwork` DaemonSet (envoy/nginx) binding
  **`10.200.0.1:443`**, TLS-passthrough or SNI-routing to the admin backends (or to
  the gateway ClusterIP). Binds the `wg0` socket directly, so it **sidesteps both**
  the Cilium device-pinning and the masquerade problem.
- Admin DNS → `10.200.0.1`; only the tunnel routes it → **VPN-only by
  reachability** (unroutable off the tunnel — no public path to close at the LB,
  though public DNS should still drop the admin names).
- **Pro:** explicit, predictable datapath; **no core Cilium change**; cleanest
  VPN-only property. **Con:** a new component to run on the CPs; must forward TLS to
  the existing gateway (passthrough) so it doesn't re-implement certs/oauth2-proxy.

### C — Add `wg0` to Cilium `devices` + a dedicated internal Gateway
- Put `wg0` in Cilium `devices` (tunnel enters the BPF datapath), add a second
  `Gateway platform-internal` with an LB-IPAM pool on the tunnel subnet, move admin
  routes there.
- **Pro:** native-Cilium end-state, clean separation. **Con:** changing `devices`
  re-attaches BPF **cluster-wide** on live prod (highest blast radius); most moving
  parts. **Not a first move.**

## The two cruxes — validate against the LIVE tunnel before committing

1. **Datapath reachability.** With the tunnel up: can a home client reach the
   gateway at `10.0.1.7` (A) or a `wg0`-bound proxy (B)? Observe the source IP a
   backend sees, the return path, and whether forwarding/masquerade is needed.
2. **Public-exposure closure.** Confirm that removing the admin hostnames from
   **Cloudflare** DNS (and/or the public gateway listener) fully closes external
   access while Dex stays reachable for the OIDC redirect.

Neither can be answered until the listener (#2462) is applied and the tunnel is up
— hence design-first.

## Recommendation & sequence

**Approach B** is the most predictable, lowest-blast-radius path to *true*
VPN-only: access is by reachability to `10.200.0.1`, which is unroutable off the
tunnel, and it avoids both hard constraints. A is the lightest if CP
forwarding/masquerade + public-DNS removal prove clean; C is the tidy end-state but
too risky to attempt first on live prod.

1. **Land the listener** (#2462) — additive, config-only.
2. **Maintainer brings the tunnel up** — generate the server keypair; apply the
   tenant so the gateway mints its key; set the OpenBao WG secrets
   (`infrastructure/unifi/wireguard`); `talosctl --nodes 10.0.1.1,10.0.1.2,10.0.1.3
   patch mc` the listener.
3. **Validate the datapath** (crux 1) from a tunnel client — read the observed
   behavior, don't assume it.
4. **Implement Approach B** as a draft PR (CP `hostNetwork` proxy on `wg0`), then
   cut admin DNS over to `10.200.0.1` and **validate VPN-only end-to-end** (crux 2:
   close Cloudflare/public exposure), one service at a time, Headlamp or Longhorn
   first (lowest blast radius), OpenBao last.

## Open decisions for the maintainer

- **Approach A vs B** (this doc recommends **B**).
- **Public DNS:** drop the admin hostnames from Cloudflare entirely (VPN-only), or
  keep them public-but-app-auth-gated as a fallback? True "require VPN" means
  dropping them.
- **Scope:** all seven at once, or start with the two lowest-risk (Headlamp,
  Longhorn) and expand? (OpenBao last — losing access to it mid-change is the worst
  case.)
