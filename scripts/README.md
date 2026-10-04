# Operational scripts

## Consumer discovery conservation

`guard-consumer-discovery-conservation.sh` compares the publish-revision
report's file scan with the production overlay and every Flux root it names.
It renders locally with `kubectl kustomize`; it uses no cluster, credentials
or network. Run the guard and its fixture regressions from the repository root:

```bash
bash scripts/guard-consumer-discovery-conservation.sh
bash scripts/tests/test-guard-consumer-discovery-conservation.sh
```

The comparison requires the KSail-generated platform artifact and its current
`latest` reference. An omitted or empty OCI reference uses Flux's `latest`
default; explicit semver or digest selection, and patches selecting another
reference, cannot attest this checkout. Tenant sources retain their own
references. The guard also refuses suspended production roots and paths that
resolve outside the published `k8s/` tree, including symlink escapes.

Non-empty ResourceSet `resourcesTemplate` strings, including step templates,
are refused because their controller evaluates them after this static render.
For OCI-templating kro ResourceGraphDefinitions, the guard counts instances by
schema group (default `kro.run`), API version and kind. Unrelated resources with
the same kind remain valid peers; a matching instance requires runtime consumer
discovery that this check does not provide.

A passing guard proves agreement for this repository's supported static
declarations. It does not prove production convergence or discover objects
inside tenant artifacts, Helm charts or controller-generated templates.
Unknown input fails the check; it is never treated as an empty agreeing set.

Reference semantics: [Flux OCI references](https://fluxcd.io/flux/components/source/ocirepositories/#reference),
[Flux root paths and suspension](https://fluxcd.io/flux/components/kustomize/kustomizations/),
[ResourceSet templates](https://fluxoperator.dev/docs/crd/resourceset/#resources-template),
and [kro schema GVK](https://kro.run/api/crds/resourcegraphdefinition/).
