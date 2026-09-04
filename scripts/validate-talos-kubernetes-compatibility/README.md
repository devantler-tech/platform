# Kubernetes/Talos compatibility check

Run from the repository root:

```sh
go run ./scripts/validate-talos-kubernetes-compatibility ksail.prod.yaml
go test ./scripts/validate-talos-kubernetes-compatibility
```

The check reads the explicit Kubernetes and Talos pins from one KSail YAML
document. It calls the versioned Talos machinery library's
`KubernetesVersion.SupportedWith` predicate, the same predicate used by Talos
runtime configuration validation. It starts no cluster, reads no credentials,
and does not generate machine secrets.

`talosctl validate`, including `--strict`, does not invoke runtime validation;
it accepts the incompatible Kubernetes v1.37.0 / Talos v1.13.9 pairing offline.
The regression tests exercise that exact pairing and the supported v1.36.4
control through the upstream compatibility predicate instead.

The required Talos validation job runs this check when production pins, its
validator, the Talos installer, or Go dependency inputs change. Unknown Talos
release families fail closed; update the reviewed machinery dependency when
adopting a release it does not yet understand. No compatibility table is copied
into this repository.

Passing this check proves a declared version pairing only. It does not prove
the fleet has completed an OS rollout or authorize merging a staged upgrade.
A Talos OS upgrade must still land and finish separately before Kubernetes is
raised, as documented beside the production pins.
