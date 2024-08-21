# Welcome to Devantler's Homelab 🚀

<details>
  <summary>Show/hide folder structure</summary>

<!-- readme-tree start -->
```
.
├── .github
│   └── workflows
├── .vscode
├── k8s
│   ├── apps
│   │   ├── fleetdm
│   │   ├── homepage
│   │   ├── open-webui
│   │   └── plantuml
│   ├── clusters
│   │   ├── homelab-local
│   │   │   ├── flux-system
│   │   │   └── variables
│   │   └── homelab-prod
│   │       ├── custom-resources
│   │       │   └── gha-runner-scale-sets
│   │       ├── flux-system
│   │       ├── infrastructure
│   │       │   └── cilium
│   │       └── variables
│   ├── custom-resources
│   │   ├── middlewares
│   │   │   ├── basic-auth
│   │   │   └── forward-auth
│   │   └── selfsigned-cluster-issuer
│   ├── distributions
│   │   ├── k3s
│   │   │   └── variables
│   │   └── talos
│   │       ├── infrastructure
│   │       │   ├── kubelet-serving-cert-approver
│   │       │   └── longhorn
│   │       └── variables
│   ├── infrastructure
│   │   ├── capi-operator
│   │   ├── cert-manager
│   │   ├── cloudflared
│   │   ├── gha-runner-scale-set-controller
│   │   ├── goldilocks
│   │   ├── harbor
│   │   ├── helm-charts-oci-proxy
│   │   ├── k8sgpt-operator
│   │   ├── kyverno
│   │   ├── metrics-server
│   │   ├── oauth2-proxy
│   │   ├── ollama
│   │   ├── reloader
│   │   ├── traefik
│   │   └── trivy-operator
│   ├── tenants
│   └── variables
└── talos
    ├── hetzner
    └── patches
        ├── cluster
        └── nodes

56 directories
```
<!-- readme-tree end -->

</details>

![image](https://github.com/user-attachments/assets/cc96e95c-4362-4432-9509-7f52c6c21636)

This repo contains the deployment artifacts for Devantler's Homelab. The Homelab is a Kubernetes cluster that is highly automated with the use of Flux GitOps, CI/CD with Automated Testing, and much more. Feel free to look around. You might find some inspiration 🙌🏻

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

- [Cluster API Operator](https://cluster-api.sigs.k8s.io/) - To manage the lifecycle of tenant clusters.
- [Cert Manager](https://cert-manager.io/docs/) - For managing certificates in the cluster.
- [Cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) - To tunnel all traffic through Cloudflare, and to keep my network private.
- [GitHub Actions Runner Scale Sets](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners-with-actions-runner-controller/about-actions-runner-controller) - To run GitHub Actions in the cluster.
- [Goldilocks](https://goldilocks.docs.fairwinds.com) - To recommend and update Vertical Pod Autoscaler requests and limits.
- [Harbor](https://goharbor.io) - To store and manage container images.
- [Homepage](https://gethomepage.dev/) - To provide a dashboard for the cluster.
- [K8sGPT Operator](https://k8sgpt.ai) - To analyze and optimize the cluster for errors and improvements.
- [Metrics Server](https://kubernetes-sigs.github.io/metrics-server/) - To collect and expose metrics from the cluster.
- [OAuth2 Proxy](https://oauth2-proxy.github.io/oauth2-proxy/) - To proxy authentication requests to upstream OAuth2 providers.
- [Ollama](https://ollama.com) - To run LLM's in the cluster, and to provide a REST API to access them remotely.
- [Open WebUI](https://openwebui.com) - A web interface to interact with Ollama and OpenAI's AI models.
- [PlantUML](https://plantuml.com) - To generate UML diagrams from text.
- [Traefik](https://doc.traefik.io/traefik/) - To route traffic to the correct services in the cluster.
- [Trivy Operator](https://aquasecurity.github.io/trivy-operator/latest/) - To continuously scan your Kubernetes cluster for security issues.

## Cluster Configuration

The cluster uses Flux GitOps to reconcile the state of the cluster with single source of truth stored in this repository and published as an OCI image. For development, the cluster is spun up by `KSail` and for production, the cluster is provisioned by `Talos Omni`.

The cluster configuration is storen in the `k8s/*` directories where the structure is as follows:

- `clusters/*`: Contains the the cluster specific configuration for each environment. For example entry-level Flux kustomizations, and the environment specific variables.
- `distributions/*`: Contains the distribution specific configuration. For example distribution specific variables, and infrastructure components needed to support the distribution. Talos for example does not have a built-in kubelet-serving-cert-approver, so it is required to make metrics server access kubelet with a certificate.
- `apps/*`: Contains the application specific manifests. For example the homepage, local-ai, ollama, and plantuml.
- `infrastructure/*`: Contains the infrastructure specific manifests. For example cert-manager, cloudflared, gha-runner-scale-set-controller, goldilocks, harbor, k8sgpt-operator, metrics-server, oauth2-proxy, and traefik.
- `repositories/*`: Contains the repositories that are used by the cluster. For example the `flux-system` repository.
- `tenants`: Contains Flux kustomizations to bootstrap and onboard tenants. (currently not in use)
- `variables/*`: Contains global variables, that are the same for all clusters.

## Production Environment

### Nodes

- 2x [Hetzner CAX21 nodes](https://www.hetzner.com/cloud/) (QEMU ARM64 4CPU 8Gb RAM 80Gb SSD) for the control plane and worker nodes
- 1x [Hetzner CAX41 node](https://www.hetzner.com/cloud/) (QEMU ARM64 16CPU 32Gb RAM 320Gb SSD) for the control plane and worker node
- 1x [UTM](https://mac.getutm.app) Apple Hypervisor ARM64 VM (Running on Mac Mini M2 Pro with access to 32GB RAM and 10 cores) as a worker node
  - The Apple Hypervisor performs better than QEMU, and is thus preferred for the worker nodes.
- 1x [UTM](https://mac.getutm.app) QEMU ARM64 VM (Running on Mac Mini M2 Pro with access to 2GB RAM and 2 cores) as a worker node
  - The QEMU VM is used to attach external disks to the cluster. This is not supported by Apple Hypervisor.

### Hardware

- [Unifi Cloud Gateway](https://eu.store.ui.com/eu/en/pro/products/ucg-ultra) - For networking and firewall.
- [External Samsung T5/T7 SSD Disks](https://www.samsung.com/dk/memory-storage/portable-ssd/portable-ssd-t7-1tb-gray-mu-pc1t0t-ww/) - For distributed storage across the cluster.

### Software

- [Unifi](https://ui.com/) - For configuring a DMZ zone for my own nodes to run in, along with other security features.
- [Talos Omni](https://www.siderolabs.com/platform/saas-for-kubernetes/) - For provisioning the production cluster, and managing nodes, updates, and the Talos configuration.
- [Cloudflare](https://www.cloudflare.com) - For etcd backups, DNS, and tunneling all traffic so my network stays private.
- [Flux GitOps](https://fluxcd.io) - For managing the kubernetes applications and infrastructure declaratively.
- [SOPS](https://getsops.io) and [Age](https://github.com/FiloSottile/age) - For encrypting secrets at rest, allowing me to store them in this repository with confidence.
- [KSail](https://github.com/devantler/ksail) - For developing the cluster locally, and for running the cluster in CI to ensure all changes are properly tested before being applied to the production cluster.
- [K8sGPT](https://k8sgpt.ai) - To analyze the cluster for improvements, vulnerabilities or bugs. It integrates with Trivy and Kuverno to also provide security and policy suggestions.
