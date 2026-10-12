# Infrastructure

Services the rest of the platform depends on: networking, certificates,
policy, secrets, single sign-on, storage, observability and backups.

Flux applies this directory as two layers, in order:

1. [`controllers/`](controllers) — one folder per component, holding its Helm
   release and the resources that belong to it. Most install the custom resource
   types that the second layer uses.
2. Everything else in this directory — resources that need those types to exist
   first (for example [`cluster-policies/`](cluster-policies) for Kyverno and
   [`tracing-policies/`](tracing-policies) for Tetragon), and components that
   need a controller to be running, such as [`gateway/`](gateway) and
   [`coroot/`](coroot).

Each layer's `kustomization.yaml` lists what the layer includes. Production
includes everything those files list and adds its own resources. The local cluster
includes a small core set and leaves the rest as commented lines to opt in to;
see [local development](../../../docs/local-development.md).

Where a resource belongs, and how its file is named, is described in
[AGENTS.md](../../../AGENTS.md#infrastructure-file-structure-convention).
