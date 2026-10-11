# Devantler Tech Platform ☸️⛴️

A self-hosted Kubernetes platform for Devantler Tech's business services and
personal applications. This repository contains the configuration for that live
platform, shared as a reference—not a ready-to-use distribution.

Want to build a similar platform for yourself? Start with
[platform-template](https://github.com/devantler-tech/platform-template), the
reusable template for creating your own instance.

Changes are reviewed in Git. [Flux](https://fluxcd.io) applies the published
configuration to the cluster automatically.
[KSail](https://github.com/devantler-tech/ksail) manages the Talos clusters and
the workload lifecycle.

## What's included

- **Compute:** Talos Linux on Hetzner Cloud, with a smaller Docker-based local
  test environment.
- **Networking:** Cilium, Gateway API, Cloudflare DNS, and certificate
  management.
- **Access and secrets:** Dex single sign-on, OpenBao, External Secrets, and
  SOPS-encrypted bootstrap secrets.
- **Storage:** Longhorn volumes and CloudNativePG-managed PostgreSQL.
- **Operations:** Coroot observability, autoscaling, progressive delivery, and
  backup/recovery tooling.
- **Applications:** shared platform services and tenant apps deployed from their
  own repositories.

The local environment includes only core infrastructure by default; applications
and heavier components are opt-in. See the
[architecture guide](docs/architecture.md) for the component inventory,
environment differences, and how the layers fit together.

## Start here

- **Explore the design:** read the [architecture guide](docs/architecture.md),
  then browse [shared components](k8s/bases),
  [provider overlays](k8s/providers), and [cluster definitions](k8s/clusters).
- **Build your own platform:** follow the template's
  [bootstrap guide](https://github.com/devantler-tech/platform-template/blob/main/docs/BOOTSTRAP.md).
  To adapt this reference instance directly instead, see
  [customization](docs/TEMPLATING.md) and
  [local development](docs/local-development.md). Its committed secrets cannot
  be decrypted with your keys; replace them with your own encrypted values.
- **Host an application:** follow [tenant onboarding](docs/TENANTS.md).
- **Contribute a change:** read [contributor guidance](AGENTS.md) and run the
  [validation checks](docs/validation.md). Validation does not require starting
  a cluster.

## Operating the platform

- [Cluster access](docs/oidc-kubectl.md)
- [Secrets and rotation](docs/secret-rotation.md)
- [Storage and data retention](docs/deletion-and-data-retention.md)
- [Backups and disaster recovery](docs/dr/README.md)
- [Production recovery](docs/operations/production-recovery.md)

More topic-specific guides and design decisions live in [docs/](docs).
