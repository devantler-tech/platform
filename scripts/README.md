# Operational scripts

Go commands and Bash scripts that check this repository or operate the
platform. They fall into three groups, and only the first is meant for a
contributor's machine.

## Checks you can run without a cluster or credentials

Run these from the repository root. Each reads only files in your checkout.
You need Go (the version in [`go.mod`](../go.mod)); the first run downloads the
Go modules the commands use.

| Check                                                                             | What it catches                                                        |
| --------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| [`validate-embedded-json`](validate-embedded-json/README.md)                      | Broken JSON inside a ConfigMap, which schema checks read as plain text. |
| [`validate-homepage-bookmarks`](validate-homepage-bookmarks/README.md)            | Homepage links without HTTPS or an icon, and duplicate or colliding groups. |
| [`validate-feature-flags`](validate-feature-flags/README.md)                      | Invalid feature-flag definitions.                                      |
| [`validate-talos-kubernetes-compatibility`](validate-talos-kubernetes-compatibility/README.md) | A Kubernetes version the pinned Talos version does not support.        |
| [`guard-bash-local-initialization`](guard-bash-local-initialization/README.md)    | Bash functions that read a local variable before setting it.           |
| [`guard-consumer-discovery-conservation.sh`](#consumer-discovery-conservation)    | Disagreement between two views of what production deploys. Also needs `kubectl`. |

Most scripts have a regression test: a Go test beside the command
(`go test ./scripts/<name>`) or a `test-*.sh` file in [`tests/`](tests).
[Validation](../docs/validation.md) covers the manifest checks, and
[`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml) shows which checks
CI runs for which paths.

## Tools that support other checks

- [`render-helm-chart`](render-helm-chart/README.md) builds the Helm renderer
  that the post-renderer and disruption-budget guards use.
- [`reconcile-talos-kernel-args`](reconcile-talos-kernel-args/README.md) is
  part of the offline Talos configuration check.
- [`tests/fixtures/cosign-verify-failures`](tests/fixtures/cosign-verify-failures/README.md)
  holds inputs for a diagnostic classifier.

## Operator procedures and experiments

These read or change the running platform, or use its credentials. Do not run
them as part of a contribution. They are documented for the maintainer and as a
record of how a procedure works.

- [`audit-storage-retention`](audit-storage-retention/README.md) — read-only
  audit of a snapshot captured with production read access. Its `go test`
  needs no access.
- [`validate-break-glass`](validate-break-glass/README.md) — checks an export
  of the emergency-access entry without printing it.
- [`verify-arc-app-identity`](verify-arc-app-identity/README.md) — proof run by
  a manual workflow. Its `--preflight` mode alone needs no credentials.
- [`prove-scoped-package-access`](prove-scoped-package-access/README.md) — a
  manual CI experiment on registry access.
- [`replace-wedding-standby`](replace-wedding-standby/README.md) — a
  production database repair that needs the maintainer's approval for each run.

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
