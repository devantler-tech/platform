# Local development

This is a small Talos-on-Docker test environment, not a copy of production.
Commands below are for your own adapted instance; do not use them against the
maintained production cluster.

## Prerequisites

Install these tools:

- [Docker](https://docs.docker.com/get-docker/) — runs the cluster on your
  machine.
- [KSail](https://github.com/devantler-tech/ksail) — creates and manages that
  cluster. CI uses the same tool to validate and security-scan manifests, but
  does not boot a cluster; running one is a local step.
- [SOPS](https://getsops.io) and [Age](https://github.com/FiloSottile/age) —
  encrypt your own secret values.

## Prepare your instance

> [!IMPORTANT] The committed secrets are encrypted for the maintained instance,
> so you cannot decrypt them. To run your own instance:
>
> 1. Fork this repo.
> 2. Create your own Age keys.
> 3. Update `.sops.yaml` in the repository root.
> 4. Replace every `*.enc.yaml` file under `k8s/` with your own values,
>    encrypted for your Age key. Changing the recipients alone cannot decrypt
>    the original values.
> 5. Supply your Age key to local SOPS and, if using Actions, your fork's GitHub
>    secrets as described in the [forking guide](TEMPLATING.md).

## Start the cluster

After replacing the instance-specific configuration and secrets in the
[forking guide](TEMPLATING.md), run:

```bash
ksail cluster create
ksail workload push
ksail workload reconcile
```

Ports 80 and 443 are mapped to localhost (via `extraPortMappings` in
[`ksail.yaml`](../ksail.yaml)). Add the relevant entries from
[`hosts`](../hosts) to your machine's hosts file so the local service names
resolve to `127.0.0.1`. The repository's hosts file does not configure name
resolution by itself.

The local cluster is a **thin test-bed**: somewhere to try one component before
promoting it to production, not a copy of production. It starts with core
infrastructure only — networking and gateway, DNS, TLS, Flux, policy, vertical
pod autoscaling, secrets, single sign-on, and the CloudNativePG operator. Note
that the operator is installed but no database is created: apps are opt-in, so a
plain local bring-up has no PostgreSQL instance running.

Everything heavier (observability, request-rate autoscaling, backup, runtime
security, the VM stack, …) and all apps are opt-in. For infrastructure,
uncomment the desired controllers, matching resources, and patches. For apps,
replace `resources: []` with the desired entries and add any required patches.
These files carry copy-paste templates; after editing them, re-run
`ksail workload push && ksail workload reconcile`:

- [`k8s/providers/docker/infrastructure/controllers/kustomization.yaml`](../k8s/providers/docker/infrastructure/controllers/kustomization.yaml)
- [`k8s/providers/docker/infrastructure/kustomization.yaml`](../k8s/providers/docker/infrastructure/kustomization.yaml)
- [`k8s/providers/docker/apps/kustomization.yaml`](../k8s/providers/docker/apps/kustomization.yaml)

## Clean up

Remove only the local test cluster:

```bash
ksail cluster delete
```

For checks that need no running cluster, see [validation](validation.md).
