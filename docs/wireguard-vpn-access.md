# WireGuard VPN: the bridge into the home network

The WireGuard VPN connects the Platform to the home network, so integrations running on the Platform
can reach the maintainer's own hardware and appliances. It is not how internal web pages are
protected: a zero-trust sign-on gate does that.

The decision behind this design is recorded in
[platform#5001](https://github.com/devantler-tech/platform/issues/5001).

## Two ways in

Everything on the home network that the Platform or the maintainer needs to reach uses exactly one
of two paths.

| Path | What it is for | How it is protected |
| --- | --- | --- |
| **Zero trust** (the default) | Internal web UIs, and every appliance judged safe to expose | The Platform's single sign-on gate checks who is asking before a request reaches the service. |
| **VPN-only** | Appliances identified as compromisable | Reachable only through the WireGuard tunnel. Nothing about them is published to the internet. |

Zero trust is the default because it is the easier path to use, and current zero-trust security is
strong enough to protect the home network when the service behind it is sound. VPN-only is the
exception, for anything that should not face the internet even behind a sign-on gate.

## Web access goes through zero trust

The VPN never decides who may open an internal web UI. Every internal web UI belongs behind the
zero-trust gate. One that is not behind it yet is put behind it, or kept VPN-only until it is.

## How the bridge works

The tunnel does not carry traffic yet; the [status](#status) table shows what is in place.

- **The Platform runs the server end of the tunnel**, defined in the Talos
  [`wireguard.yaml`](../talos/control-planes/wireguard.yaml) patch.
- **The home gateway is the client and dials out to the Platform.** The home side starts the
  connection, so a changing home internet address never matters. The gateway's side is managed
  declaratively by the [`devantler-tech/unifi`](https://github.com/devantler-tech/unifi) tenant.
- **Only the routes integrations need go through the tunnel.** Nothing else on the home network is
  reachable through it, so an appliance cannot become reachable by accident.
- **WireGuard ignores anything it cannot authenticate.** A packet from an unknown key gets no reply,
  so the server end gives nothing away to a scan.
- **Keys never live in Git.** The server's private key is a deployment secret, and the gateway's
  keys are held in OpenBao, as [`unifi-management.md`](unifi-management.md) describes.

## Choosing an appliance's path

Each appliance that an integration or the maintainer needs gets an explicit decision:

1. **Start with zero trust** when the appliance can be exposed safely behind the sign-on gate.
2. **Move it to VPN-only quickly** as soon as it is identified as compromisable. This is a small,
   fast change: take away the appliance's public route, so that only the tunnel reaches it.
3. **Move it back only with evidence** that it is safe to expose again.

The record of which appliance uses which path is kept privately, outside this repository, with the
other network values that [unifi#80](https://github.com/devantler-tech/unifi/issues/80) keeps out of
public files.

## Status

| Piece | State | Tracked in |
| --- | --- | --- |
| Server end of the tunnel | In place, with no peer registered, so nothing can connect yet | [platform#2462](https://github.com/devantler-tech/platform/pull/2462) |
| Home gateway registered as the tunnel's peer | Open | [platform#2473](https://github.com/devantler-tech/platform/issues/2473) |
| Home gateway client and its DNS records | Defined in the UniFi tenant and paused | [unifi#2](https://github.com/devantler-tech/unifi/issues/2) |
| Every internal web UI confirmed behind the zero-trust gate | Open | [platform#5001](https://github.com/devantler-tech/platform/issues/5001) |
| Private per-appliance exposure record, with a quick way to move one appliance to VPN-only | Open | [platform#5001](https://github.com/devantler-tech/platform/issues/5001) |

The UniFi tenant's paused DNS records for admin web UIs are retired, or limited to appliances kept
VPN-only, before its pause is lifted.
