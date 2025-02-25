# Welcome to the DevantlerTech Platform ⛴️

> [!WARNING]
> I am currently working towards making my other OS project [KSail](https://github.com/devantler/ksail) work really well for my homelab. As such this repo will not see much development the coming weeks. Rest assured, that it will recieve some much needed love, where I share my latest and greatest learnings in the Platform Engineering space.

<img width="1840" alt="Screenshot 2024-09-03 at 00 51 44" src="https://github.com/user-attachments/assets/eb6729f7-edff-4346-9be9-0c77d9740633">

This repo contains the deployment artifacts for the DevantlerTech Platform. The platform is a Kubernetes cluster that is highly automated with the use of Flux GitOps, CI/CD with Automated Testing, and much more. Feel free to look around. You might find some inspiration 🙌🏻

<details>
  <summary>Show/hide folder structure</summary>

<!-- readme-tree start -->
```text
.
├── docs
├── hetzner
├── k8s
│   ├── clusters
│   │   ├── homelab-local
│   │   └── homelab-prod
│   ├── components
│   │   ├── flux-kustomization-post-build-variables-label
│   │   ├── flux-kustomization-sops-label
│   │   ├── helm-release-crds-label
│   │   ├── helm-release-helm-test-label
│   │   ├── helm-release-remediation-label
│   │   └── network-policy-default-deny
│   ├── distributions
│   │   ├── k3s
│   │   └── talos
│   └── shared
│       ├── apps
│       ├── infrastructure
│       ├── tenants
│       └── variables
└── talos
    └── patches
        ├── cluster
        └── nodes

26 directories
```
<!-- readme-tree end -->

</details>

## Prerequisites

For development:

- [Docker](https://docs.docker.com/get-docker/)
- [KSail](https://github.com/devantler/ksail)

For production:

- A Talos Cluster

> [!NOTE]
> You can use other distributions as well, but the configuration is optimized for Talos, and thus it is not guaranteed to work with other distributions.

## Usage

To run this cluster locally, simply run the following command:

```bash
ksail up homelab-local
```

> [!NOTE]
> To run this cluster on your metal, would require that you have access to my SOPS keys. This is ofcourse not possible, so you would need to create your own keys and replace the existing ones, if you want to run my cluster configuration on your own metal.
>
> - The keys that `KSail` uses are stored in `~/.ksail/age` where one Age key is store for each cluster, and named according to the cluster name. For example `~/.ksail/age/homelab-local`.
> - To update SOPS to work with `Ksail`, you need to update the `.sops.yaml` file in the root of the repository, and replace the `age` keys with your own keys.
> - To update the manifests to work with `KSail`, you need to replace all `.sops.yaml` files with new ones, that are encrypted with your own keys.
>
> For the production cluster, you would need to do the same, but in addition to storing the keys in `~/.ksail/age`, you would also need to store the keys in GitHub Secrets, such that the CI/CD pipeline can provision the keys to the cluster.

## Stack

The cluster uses Flux GitOps to reconcile the state of the cluster with single source of truth stored in this repository and published as an OCI image. For development, the cluster is spun up by `KSail` and for production, the cluster is provisioned by `Talos Omni`.

The cluster configuration is stored in the `k8s/*` directories where the structure is as follows:

- [`clusters/`](k8s/clusters): Contains the the cluster specific configuration for each environment.
- [`components/`](k8s/components): Contains the reusable components that are used across the cluster.
- [`distributions/`](k8s/distributions): Contains the distribution specific configuration.
- [`shared/`](k8s/shared): Contains the shared configuration for all clusters.
  - [`apps/`](k8s/shared/apps): Contains the application specific manifests.
    - [FleetDM](k8s/shared/apps/fleetdm) - To provide a device management for my devices. (currently not in use, as it does not support ARM64)
    - [Headlamp](k8s/shared/apps/headlamp) - To provide a lightweight and extensible Kubernetes UI.
    - [Homepage](k8s/shared/apps/homepage) - To provide a dashborad for the cluster.
    - [Open WebUI](k8s/shared/apps/open-webui) - To provide a web interface and a REST API for interacting with LLM's.
    - [PlantUML](k8s/shared/infrastructure/plantuml) - To provide a web interface and a REST API for generating PlantUML diagrams.
    - [Traefik](k8s/shared/infrastructure/controllers/traefik) - To provide an ingress controller for the cluster.
  - [`custom-resources/`](k8s/shared/custom-resources): Contains the custom resources that are used across the cluster.
    - [Middlewares](k8s/shared/infrastructure/middlewares) - Contains the middlewares that are used by Traefik.
    - [Selfsigned Cluster Issuer](k8s/shared/infrastructure/selfsigned-cluster-issuer) - Contains the selfsigned cluster issuer that is used by Traefik.
  - [`infrastructure/`](k8s/shared/infrastructure): Contains the infrastructure specific manifests.
    - [Cert Manager](k8s/shared/infrastructure/cert-manager) - For managing certificates in the cluster.
    - [Cloudflared](k8s/shared/infrastructure/cloudflared) - For tunneling traffic to the cluster.
    - [Dex](k8s/shared/infrastructure/controllers/dex) - For providing OIDC authentication for the cluster.
    - [Cluster API Operator](k8s/shared/infrastructure/capi-operator) - For managing the lifecycle of Kubernetes clusters.
    - [GitHub Actions Runner Scale Set Controller](k8s/shared/infrastructure/gha-runner-scale-set-controller) - To manage GitHub Actions Runner Scale Sets in the cluster.
    - [GitHub Actions Runner Scale Sets](k8s/clusters/homelab-prod/infrastructure/gha-runner-scale-sets) - To run GitHub Actions in the cluster.
    - [Goldilocks](k8s/shared/infrastructure/controllers/goldilocks) - To provide and apply resource recommendations for pods.
    - [Harbor](k8s/shared/infrastructure/harbor) - To store and distribute container images.
    - [K8sGPT Operator](k8s/shared/infrastructure/controllers/k8sgpt-operator) - To analyze the cluster for improvements, vulnerabilities or bugs.
    - [Kyverno](k8s/shared/infrastructure/controllers/kyverno) - To enforce policies in the cluster.
    - [Longhorn](k8s/distributions/talos/core/longhorn) - To provide distributed storage for the cluster.
    - [Metrics Server](k8s/shared/infrastructure/controllers/metrics-server) - To provide metrics for the cluster.
    - [OAuth2 Proxy](k8s/shared/infrastructure/oauth2-proxy) - To provide authentication for the cluster.
    - [Ollama](k8s/shared/infrastructure/ollama) - To run LLM's on the cluster.
    - [Reloader](k8s/shared/infrastructure/controllers/reloader) - To reload deployments when secrets or configmaps change.
    - [Testkube](k8s/shared/infrastructure/controllers/testkube) - To provide a testing framework for the cluster.
    - [Trivy Operator](k8s/shared/infrastructure/controllers/trivy-operator) - To analyze the cluster for vulnerabilities.
  - [`tenants`](k8s/shared/tenants): Contains Flux kustomizations to bootstrap and onboard tenants. (currently not in use)
  - [`variables/`](k8s/shared/variables): Contains global variables, that are the same for all clusters.

## Kustomize and Flux Kustomization Flow

To support hooking into the kustomize flow for adding or modifying resources for a specific cluster, a specific distribution, or shared across all clusters, the following structure is used:

![Structure](docs/images/gitops-structure.drawio.png)

This means that for every root level kustomization that is applied to the cluster, there should be a corresponding folder in either `clusters`, `distributions`, or `shared` that contains the resources that should be applied to the cluster at that scope. For example, for a root level kustomization in `k8s/clusters/<cluster-name>/flux-system/infrastructure.yaml`, there should be a corresponding folder in:

- `k8s/clusters/<cluster-name>/infrastructure/`
- `k8s/distributions/<distribution-name>/infrastructure/`
- `k8s/shared/infrastructure/`

## Production Environment

### Nodes

- 1x [Hetzner CAX21 node](https://www.hetzner.com/cloud/) (QEMU ARM64 4CPU 8Gb RAM 160Gb SSD) for both control plane and worker node
- 2x [Hetzner CAX41 node](https://www.hetzner.com/cloud/) (QEMU ARM64 16CPU 32Gb RAM 320Gb SSD) for both control plane and worker nodes
- 1x Apple Hypervisor ARM64 VM (Running on Mac Mini M2 Pro with access to 32GB RAM and 20 cores (overprovisioned 2/1) as a worker node

### Hardware

- [Unifi Cloud Gateway](https://eu.store.ui.com/eu/en/pro/products/ucg-ultra) - For networking and firewall.
- [External Samsung T5/T7 SSD Disks](https://www.samsung.com/dk/memory-storage/portable-ssd/portable-ssd-t7-1tb-gray-mu-pc1t0t-ww/) - For distributed storage across the cluster.

### Software

- [Unifi](https://ui.com/) - For configuring a DMZ zone for my own nodes to run in, along with other security features.
- [UTM](https://mac.getutm.app) - For running Kubernetes on Mac Mini via Apple Hypervisor.
- [Talos Omni](https://www.siderolabs.com/platform/saas-for-kubernetes/) - For provisioning the production cluster, and managing nodes, updates, and the Talos configuration.
- [Cloudflare](https://www.cloudflare.com) - For etcd backups, DNS, and tunneling all traffic so my network stays private.
- [Flux GitOps](https://fluxcd.io) - For managing the kubernetes applications and infrastructure declaratively.
- [SOPS](https://getsops.io) and [Age](https://github.com/FiloSottile/age) - For encrypting secrets at rest, allowing me to store them in this repository with confidence.
- [KSail](https://github.com/devantler/ksail) - For developing the cluster locally, and for running the cluster in CI to ensure all changes are properly tested before being applied to the production cluster.
- [K8sGPT](https://k8sgpt.ai) - To analyze the cluster for improvements, vulnerabilities or bugs. It integrates with Trivy and Kuverno to also provide security and policy suggestions.

### Monthly Cost

| Item               | No. | Per unit | Total      |
| ------------------ | --- | -------- | ---------- |
| Hetzner CAX21      | 3   | 7,49€    | $24,9      |
| Hetzner CAX41      | 1   | 29,99€   | $33,23     |
| Talos Omni         | 1   | $10      | $10        |
| Cloudflare Domains | 2   | $0,87    | $1,74      |
|                    |     |          | **$69,87** |

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=devantler/homelab&type=Date)](https://star-history.com/#devantler/homelab&Date)
