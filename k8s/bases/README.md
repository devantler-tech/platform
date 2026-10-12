# Bases

Shared resources that every provider and cluster builds on. Nothing here is
applied directly: a provider overlay in [`../providers`](../providers) includes
the parts it wants, and a cluster in [`../clusters`](../clusters) selects one
provider.

| Directory                          | Holds                                                                                            |
| ---------------------------------- | ------------------------------------------------------------------------------------------------ |
| [`bootstrap/`](bootstrap)          | Values Flux substitutes into the other layers, plus the limit ranges and priority classes they need. |
| [`infrastructure/`](infrastructure) | Platform services: controllers first, then the resources that depend on them.                    |
| [`apps/`](apps)                    | Applications and tenant deployments.                                                             |
| [`components/`](components)        | Kustomize components that overlays opt in to, such as Flux defaults for every HelmRelease.       |

Flux applies the layers in order: `bootstrap`, `infrastructure/controllers`,
`infrastructure`, then `apps`.

Change a base when every consumer of the resource should get the change. For a
difference that belongs to one provider, add a patch in that provider's overlay
instead. The naming and layout rules are in
[AGENTS.md](../../AGENTS.md#file-and-directory-naming-conventions).

To check a change here without a cluster or credentials, build the layer that
includes it from the repository root, for example:

```bash
kubectl kustomize k8s/providers/hetzner/apps/
```

[Validation](../../docs/validation.md) lists every layer and the fuller checks.
