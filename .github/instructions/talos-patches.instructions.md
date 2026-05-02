---
description: "Use when editing Talos machine configuration patches in talos-local/ or talos/ directories. Covers patch structure, provider differences, and KSail expectations."
applyTo:
  - "talos-local/**"
  - "talos/**"
---
# Talos Machine Config Patches

## Directory Structure (KSail convention)

```
talos-local/     # Docker provider (local dev)
talos/           # Hetzner provider (prod)
  cluster/       # Patches applied to all nodes
  control-planes/ # Patches for control plane nodes only
  workers/       # Patches for worker nodes only
```

KSail expects this `cluster/`, `control-planes/`, `workers/` split.

## Key Local Patches

- `talos-local/cluster/cni.yaml` — Disables default CNI (KSail installs Cilium separately)
- Nodes will be NotReady until Cilium is installed — this is expected

## Provider Differences

| | Docker (local) | Hetzner (prod) |
|---|---|---|
| Config dir | `talos-local/` | `talos/` |
| CNI | Disabled, installed by KSail | Disabled, installed by KSail |
| Networking | Docker bridge, MetalLB not accessible from macOS host | Real network, Hetzner Cloud LB |
| Disk | Ephemeral | Hetzner Cloud volumes via CSI |

## Reference

- [Talos configuration reference](https://www.talos.dev/latest/reference/configuration/)
- [KSail Talos provisioner](https://ksail.devantler.tech/architecture/)
