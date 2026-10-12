# Providers

One overlay per place the platform runs. Each takes the shared
[bases](../bases) and adds what is specific to that place.

| Provider               | Used by                                                      | What it changes                                                                                                   |
| ---------------------- | ------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| [`docker/`](docker)    | The local test cluster ([`clusters/local`](../clusters/local)) | Core infrastructure only. Heavier components and all apps are opt-in.                                             |
| [`hetzner/`](hetzner)  | Production ([`clusters/prod`](../clusters/prod))             | Everything in the bases, plus production-only resources such as external DNS, Crossplane and tenant deployments.  |

Each provider has the same three layers as the bases: `bootstrap/`,
`infrastructure/` (with `infrastructure/controllers/` applied first) and
`apps/`. Differences from a base are written as patch files in a `patches/`
folder next to the `kustomization.yaml` that applies them.

A change under `hetzner/` reaches production when its pull request merges.
A change under `docker/` affects only clusters people start themselves.
Both can be checked without a cluster or credentials; see
[validation](../../docs/validation.md).
