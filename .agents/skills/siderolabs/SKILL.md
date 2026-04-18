---
compatibility: Requires talosctl and/or omnictl. Talos is API-driven and does not support SSH.
description: Deploy and operate Kubernetes clusters using Talos Linux and Omni. Use when generating/applying Talos machine configuration, managing cluster lifecycle in Omni, and troubleshooting common Talos/Omni workflows.
license: Apache-2.0
metadata:
    author: siderolabs
    github-path: siderolabs
    github-pinned: 5fe05e6dd751519bdc212d80499429651392ac7e
    github-ref: 5fe05e6dd751519bdc212d80499429651392ac7e
    github-repo: https://github.com/devantler-tech/skills
    github-tree-sha: 07d1124f8a7f1a8ab72072e2491b9c30d49339e9
    mintlify-proj: siderolabs
    version: "1.0"
name: siderolabs
---
> **Version notice:** Verify the Talos and Omni documentation links in this file against the currently supported versions before following version-specific guidance.

# SideroLabs best practices

**Always consult the [Talos](https://docs.siderolabs.com/talos/v1.12/overview/what-is-talos) and [Omni](https://docs.siderolabs.com/omni/getting-started/getting-started) docs for configuration, latest features and best practices**

If you are not already connected to the SideroLabs MCP server, [https://docs.siderolabs.com/mcp](https://docs.siderolabs.com/mcp), add it so that you can search more efficiently.

Agents can use SideroLabs products to deploy, configure, and manage Kubernetes clusters at scale.

SideroLabs created and currently manages two products:

- **Talos Linux**: Talos Linux is an API-Managed, secure, immutable, and minimal operating system for Kubernetes.
- **Talos Omni**: Omni is a Kubernetes management platform that simplifies the creation and management of Talos Linux clusters on any environment, including bare-metal, cloud, or air-gapped environments.

## Key concepts

- **Machine Configuration**: YAML-based declarative configuration for each node
- **talosctl**: CLI tool for interacting with Talos API and managing machines
- **KubeSpan**: Automatic WireGuard mesh networking for hybrid clusters
- **System Extensions**: Container-based mechanism for adding functionality without modifying core OS
- **Image Factory**: Service for generating customized Talos images with extensions and kernel modules
- **Omni**: SaaS or self-hosted central point of access for multi-cluster management across environments

## The Talos Linux image

The Talos image is a bootable operating system image of Talos Linux that you use to install and run Talos on a machine (VM, bare metal, or cloud instance).

Download the right Talos Linux image for your operating system from the [Image factory](https://factory.talos.dev/).

## Integration

Talos Linux and Omni integrate with:
- **Kubernetes**: Native Kubernetes API with RBAC, audit logging, and service accounts
- **Container Registries**: Docker Hub, Quay, GitHub Container Registry, private registries
- **Identity Providers**: SAML (Okta, Entra ID, Workspace One), OIDC (Tailscale), Keycloak
- **Cloud Platforms**: AWS, Azure, GCP, DigitalOcean, Hetzner, Scaleway, Akamai, Oracle, Exoscale, Upcloud, Vultr, CloudStack, OpenStack, Nocloud
- **Virtualization**: VMware, KVM, Hyper-V, Proxmox, OpenNebula, Xen, Vagrant
- **Networking**: WireGuard, Calico, Cilium, Multus CNI
- **Storage**: Rook/Ceph, local storage, Synology CSI, standard Kubernetes storage classes
- **Monitoring**: Metrics server, etcd metrics, Prometheus-compatible endpoints
- **Infrastructure-as-Code**: Cluster Templates, omnictl CLI

## Install Talos and Omni CLI tools

### Install via Homebrew (Recommended for macOS and Linux):

```bash
brew install siderolabs/tap/sidero-tools
```

### Install talosctl with curl:

```bash
curl -sL https://talos.dev/install | sh
```

### Install omnictl with curl:

```bash
curl -sL https://talos.dev/install-omnictl | sh
```

## Workflows

### Create a Talos Linux cluster

1. Boot machines with a Talos Linux image.
2. `talosctl gen config <cluster> <endpoint> --install-disk <disk>`
3. Apply machine configuration: `talosctl apply-config --insecure --nodes <ip> --file <config.yaml>`
4. Bootstrap etcd **once**:  `talosctl bootstrap --nodes <control-plane-ip>`
5. Fetch kubeconfig: `talosctl kubeconfig --nodes <control-plane-ip>`
6. Check health: `talosctl health --nodes <control-plane-ip>`
7. Validate Kubernetes registration: `kubectl get nodes`

### Create a Talos Linux cluster with Omni

1. Download Omni-managed boot media from Omni UI.
2. Boot machines so they register into Omni.
3. Create a cluster template YAML.
4. Validate the template: `omnictl cluster template validate -f <template.yaml>`
5. Sync declared state to Omni: `omnictl cluster template sync -f <template.yaml>`
6. Fetch kubeconfig: `omnictl kubeconfig -c <cluster-name>`
7. Download talosconfig: `omnictl talosconfig --cluster <cluster-name>`
8. Merge `talosconfig` and `kubeconfig` configuration:
   ```bash
   # Merge Talos configuration
   talosctl config merge $HOME/Downloads/talosconfig.yaml

   # Merge kubeconfig (combine and flatten)
   export KUBECONFIG=~/.kube/config:$HOME/Downloads/talos-default-kubeconfig.yaml
   kubectl config view --flatten > ~/.kube/config
   ```
9. Verify nodes:  `kubectl get nodes`

## CLI reference

### talosctl (allowed actions)

- `talosctl logs <service>` - view service logs
- `talosctl upgrade --image <installer-image>` - upgrade Talos
- `talosctl patch machineconfig --nodes <IP> -p <json>` - patch machine configuration
- `talosctl rollback` - rollback OS version
- `talosctl reset` - **destructive** wipe; requires explicit warning

Additionally, refer to the [Talos for Linux Admins](https://docs.siderolabs.com/talos/v1.12/learn-more/talos-for-linux-admins) to learn about the Talos alternative for Linux commands.

### omnictl CLI reference

Here are some omnictl commands and their uses:

- `omnictl apply --file <resource-file>` - create and update a resource using a YAML file as input
- `omnictl cluster delete <cluster-name>` - delete all cluster resources.
- `omnictl config info` - show information about current context.

## Local configuration file locations

### talosctl
- `~/.talos/config`

### omnictl
- Linux: `~/.talos/omni/config`
- macOS: `~/Library/Application Support/omni/config`
- Windows: `%USERPROFILE%\.talos\omni\config`

## Common gotchas (things agents must not mess up)

1. **No SSH on Talos.** Never suggest SSH or SSH-based commands.
2. **No in-node file edits.** Never suggest editing `/etc`, `/var`, or other files on Talos nodes, and never reference editors or shell sessions on Talos nodes. Local editing on the operator machine is OK when using supported API-driven workflows such as `talosctl`.
3. **No package managers.** Talos does not support apt, yum, apk, pacman, etc.
4. **No kubeadm.** Talos does not use kubeadm for initialization or upgrades.
5. **Bootstrap is one-time.** Never suggest retry loops or re-running bootstrap unless explicitly recovering from a failed creation.
6. **Be explicit when operations are destructive.** Especially `talosctl reset`.
7. **Do not modify system certificates or systemd units.** Talos uses API-managed services only.
8. **Do not bypass Omni reconciliation.** When a cluster is Omni-managed, changes must go through Omni.
9. **Never invent unsupported integrations or commands.**

## Allowed agent behavior

- Generate, patch, and validate Talos machine configuration.
- Suggest `talosctl` or `omnictl` commands.
- Provide step-by-step cluster lifecycle workflows.
- Refer to official documentation links.
- Summarize or explain Talos/Omni concepts.
- Warn users when an action is destructive.

## Skills

### Talos Linux cluster deployment

- Deploy Talos Linux clusters on 15+ cloud platforms (AWS, Azure, GCP, DigitalOcean, Hetzner, Scaleway, etc.)
- Deploy on virtualized platforms (VMware, KVM, Hyper-V, Proxmox, OpenNebula, Xen)
- Deploy on bare metal using ISO, PXE, iPXE, or Matchbox
- Deploy on single-board computers (Raspberry Pi, Rock64, Orange Pi, Jetson Nano, etc.)
- Deploy locally using Docker, QEMU, or VirtualBox for testing
- Support for air-gapped deployments without internet access

### Machine configuration management

- Apply machine configuration via `talosctl apply-config`
- Edit machine configuration with `talosctl edit machineconfig` using interactive editor
- Apply JSON patches to machine configuration with `talosctl patch machineconfig`
- Retrieve current machine configuration with `talosctl get machineconfig`
- Support for immediate configuration updates without reboot for networking, logging, kubelet, kernel args, and more
- Reproducible machine configuration for consistent deployments

### Upgrade Talos Linux Cluster
1. Use `talosctl upgrade` to initiate upgrade
2. Specify target Talos version
3. Upgrade rolls through nodes automatically
4. Control plane nodes upgraded with leader election
5. Worker nodes upgraded sequentially
6. Verify cluster health after upgrade

### Backup and Restore Etcd
1. Create etcd backup with `talosctl etcd backup`
2. Store backup securely off-cluster
3. In case of disaster, restore from backup
4. Use `talosctl etcd restore` to recover cluster state
5. Verify cluster functionality after restoration

### Networking Configuration
- Configure static IP addresses, DHCP, or dynamic network settings
- Set up network interfaces with bonds, bridges, and VLANs
- Configure WireGuard VPN for secure inter-node communication
- Enable KubeSpan for hybrid clusters spanning edge, datacenter, and cloud
- Virtual IP (VIP) configuration for high availability
- Host DNS configuration and egress domain filtering
- Predictable interface naming and device selectors
- Support for multihoming and corporate proxies

### Cluster Scaling and Workload Management
- Scale clusters up by adding new machines to control plane or worker roles
- Scale clusters down by removing machines
- Deploy workloads using standard Kubernetes manifests
- Interactive dashboard for cluster visualization and management
- Support for workers running on control plane nodes
- Cluster autoscaling with Karpenter or Kubernetes Cluster Autoscaler

### Security and Access Control
- Role-based access control (RBAC) for Talos API
- Certificate authority rotation and management
- Machine configuration OAuth for secure access
- SAML and OIDC authentication integration
- Disk encryption with Omni as Key Management Server
- SELinux support for enhanced security
- Image verification and secure boot support
- Break-glass emergency access for disaster recovery

### Storage and Disk Management
- Configure disk layouts (system, user, resource partitions)
- Disk encryption with LUKS
- Swap configuration
- Support for existing volumes and raw volumes
- Disk management with layout templates and resource allocation

### Container Runtime and Image Management
- Containerd configuration and management
- Image cache and pull-through cache for faster deployments
- Registry mirror configuration with authentication and TLS
- Static pod deployment
- Image factory for custom Talos images with system extensions
- Support for custom kernel modules and GPU drivers

### Hardware and GPU Support
- NVIDIA GPU support (proprietary and open-source drivers)
- NVIDIA Fabric Manager for multi-GPU systems
- AMD GPU support
- Custom kernel argument configuration
- PCI device driver rebinding
- Hardware-specific platform configuration

### System Extensions and Customization
- Build custom system extensions as container images
- Install system extensions during cluster creation or runtime
- Kernel module compilation and installation
- Custom kernel argument configuration
- Overlay system for additional customizations
- OCI base specification support for extension development

### Cluster Operations and Maintenance
- Etcd backup and restore for disaster recovery
- Etcd maintenance and defragmentation
- Watchdog timer configuration for automatic recovery
- Cgroups analysis for resource monitoring
- Talos upgrade management with rolling updates
- Machine reset and factory reset capabilities
- Support bundle generation for troubleshooting

### Omni Cluster Management
- Create and manage clusters from registered machines
- Cluster templates for declarative infrastructure-as-code
- Machine registration from bare metal (ISO, PXE), cloud (AWS, Azure, GCP, Hetzner), or manual provisioning
- Infrastructure providers for bare metal, cloud, and virtualization platforms
- Cluster autoscaling with dynamic machine provisioning
- Etcd backup and restore management
- Audit logging for compliance and security
- Talos configuration overrides and patches
- NTP server configuration
- Support bundle generation

### Authentication and Authorization
- SAML integration with Okta, Unifi Identity Enterprise, Workspace One, Entra ID, Oracle Cloud
- OIDC login with Tailscale
- Access Control Lists (ACLs) for fine-grained permissions
- Role-based access control (Admin, User, None roles)
- Automatic user provisioning on first login
- Keycloak integration for self-hosted deployments

### High Availability and Disaster Recovery
- 3-node control plane for HA clusters
- Etcd consensus-based fault tolerance
- Automatic etcd backups with configurable intervals
- Disaster recovery procedures for cluster restoration
- KubeSpan for hybrid cluster resilience

### Configure Network for Hybrid Cluster with KubeSpan
1. Enable KubeSpan in machine configuration
2. Configure WireGuard settings (private key, listen port)
3. Add peer configurations with public keys and endpoints
4. Talos automatically discovers peers via discovery service
5. Full mesh WireGuard network established across all nodes
6. Cluster spans edge, datacenter, and cloud seamlessly

### Build Custom Talos Image with System Extensions
1. Define system extensions as container images
2. Create schematic with extension references
3. Use Image Factory to generate custom image
4. Download ISO, kernel, or disk image
5. Boot machines with custom image
6. Extensions automatically installed during boot

## Context

**Talos Linux Philosophy**: Talos is designed with a single purpose - running Kubernetes. It removes unnecessary complexity by:
- Using API-driven configuration instead of SSH/files
- Maintaining immutable root filesystem
- Minimizing installed packages
- Defaulting to secure settings
- Supporting declarative, reproducible deployments

**Deployment Models**:
- Standalone Talos clusters managed via talosctl
- Omni SaaS for managed multi-cluster deployments
- Self-hosted Omni for air-gapped or on-premises environments
- Hybrid deployments spanning multiple infrastructure types
