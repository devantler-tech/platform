# Providers

One overlay per place the platform runs. Each takes the shared
[bases](../bases) and adds what is specific to that place.

| Provider               | Used by                                                      | What it changes                                                                                                   |
| ---------------------- | ------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| [`docker/`](docker)    | The local test cluster ([`clusters/local`](../clusters/local)) | Core infrastructure only. Heavier components and all apps are opt-in.                                             |
| [`hetzner/`](hetzner)  | Production ([`clusters/prod`](../clusters/prod))             | Everything the bases' `kustomization.yaml` files list, plus production-only resources such as external DNS, Crossplane and tenant deployments. |

Each provider has the same three layers as the bases: `bootstrap/`,
`infrastructure/` (with `infrastructure/controllers/` applied first) and
`apps/`. Each layer's `kustomization.yaml` applies the differences from a base
as patches: usually a file in a `patches/` folder, often inside the
component's own folder, and sometimes a short inline block.

A change under `hetzner/` reaches production when its pull request merges.
A change under `docker/` affects only clusters people start themselves.
Both can be checked without a cluster or credentials; see
[validation](../../docs/validation.md).
