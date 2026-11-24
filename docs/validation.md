# Validation

Run these checks from the repository root before proposing a change. They do not
start a cluster.

Before pushing, validate manifests with schema-aware checks and Flux variable
substitution:

```bash
# Validate local cluster manifests (default)
ksail workload validate

# Validate prod cluster manifests
ksail --config ksail.prod.yaml workload validate
```

These checks catch YAML errors, missing fields, and broken Kustomize overlays.
Passing static validation does not prove production convergence.

Start with [contributor guidance](../CONTRIBUTING.md#validation). The complete
[validation contract](../AGENTS.md#validation) covers affected Go/Bash tests and
CI path filters.

## Go module files

The required Go regression job checks every tracked `go.mod`, including nested
test helpers, on every PR and merge-group event:

```bash
bash scripts/check-go-modules-tidy.sh
bash scripts/tests/test-go-modules-tidy.sh
```

The guard uses `go mod tidy -diff` with the installed Go toolchain and workspace
mode disabled. It prints proposed changes and fails without editing `go.mod` or
`go.sum`. Run `go mod tidy` in the named module, review the changes and rerun the
guard. Missing sums, failed module discovery and failed Go commands also fail.
The intentional vulnerability fixture keeps its old Go declaration; checking
tidiness does not upgrade module Go versions.

## WebApp compiler

Run the committed WebApp and Tenant graphs through the pinned KRO compiler:

```bash
bash scripts/tests/test-webapp-compiler.sh
bash scripts/tests/test-webapp-compiler-wiring.sh
```

Tenant controls compile the complete graph and submit its generated API schema
to the real API server. They cover the release range a tenant follows, an
emergency pin and its removal, refusal of the retired digest and publisher pins,
source-only creation, later application activation and refusal to disable active
tenants, including objects stored under the earlier schema. These checks do not prove a live controller has reconciled a tenant.
They also use the pinned controller's label decorator and server-side applier
against the isolated API server. A competing writer uses the committed Kyverno
baseline body: the old shared policy is retired only after the independently
named tenant floor exists, then repeated applies must stop mutating it. This
does not run a Kyverno controller or establish production convergence.
`scripts/tests/test-tenant-source-constructors.sh` separately exercises the actual
OCIRepository verification guard against constrained constructors and mutated
graph contexts.

`scripts/tests/test-tenant-controller-namespace-coverage.sh` replays the complete
credential-store, certificate-issuer, hostname and network-policy fixture sets
under both legacy `ksail` and actual `kro` namespace ownership. Exact nonzero
allow/deny counts prevent excluded rows from masquerading as enforcement.

The fixture downloads checksum-verified Kubernetes 1.36.2 API-server assets and
starts a disposable, loopback-only API server. It explicitly refuses an existing
cluster. The KRO CRD matches the chart/library pin; the Gateway API 1.5.1 HTTPRoute
CRD supplies the fixture's route type. These are pinned test
dependencies, not an assertion about every deployed gateway version.

After real OpenAPI publication, the KRO library compiles the repository's graph.
The check requires its Deployment, Service, HTTPRoute and namespaced WebApp schema.
Unknown references, wrong output types and disabled conditional omission are
rejected. Empty or partial compiled graphs cannot count as complete coverage.
Execution is bound to all three verified binary paths, including when ambient
`TEST_ASSET_*` overrides are present. API processes and temporary state are
removed on completion, including a CRD-admission failure after process startup.
Regression controls exercise the actual binaries and confirm both listeners
have stopped before their backstop cleanup runs.

The chart reference and compiler dependency must name the same version, without
a module replacement. Keep the controller's configured feature gates intact;
the fixture does not turn an alpha feature on to make a graph compile.
Selected PR and merge-group checks fail when compilation does not succeed, and
production deployment depends on that result. Validate Main checks the merged source.

This observes admission and compilation, not controller reconciliation or running
Pods. It supplies no proof of kubelet-projected files, application readiness,
gateway reachability or private portal operation; those remain separate staging
acceptance criteria.

## Signer identities

Every first-party verifier trusts only the signing product's own release
workflow (monorepo ADR 0017, #6265). The shared reusable publishers are no
longer accepted at any revision (#6266), and the deleted legacy publisher
repository is refused at every commit (#6264). The workflow file sits directly
in `.github/workflows/` and the tag follows the strict semver grammar of the
KSail `cd.yaml` rule:

- the admission policy and the Talos node-pull rule accept any first-party
  repository whose name starts with a letter or digit, so every first-party
  repository except `.github`, which hosts reusable workflows other repositories
  call. A repository named `actions` would be a first-party repository like any
  other, so its release tag passes them; the per-tenant binding is the tenant's
  signed manifest source, which pins its image digests;
- a `Tenant` source accepts only `devantler-tech/<spec.name>`, regex-escaped;
- each registered source names its own repository literally, and `github-config`
  accepts exactly `.github`'s own `cd.yaml` on a release tag.

`scripts/guard-oci-repository-verify.sh` requires every devantler-tech
`OCIRepository` subject to name one first-party repository's own workflow: a
literal repository (never `actions`, and in `.github` only `cd.yaml`), one literal
workflow file (never `[^/]+\.ya?ml`, #6306), and a literal branch or tag ref or one of the
exact release-tag patterns the sources use. It runs on pull requests, the merge
queue, the merge-queue heal, direct pushes to `main`, the manual CD route and the
DR rebuild. The offline
controls exercise own, lookalike and other repositories, retired publisher
revisions, branches, non-release and suffixed tags and nested paths at every
layer:

```bash
bash scripts/tests/test-guard-oci-repository-verify.sh
bash scripts/tests/test-tenant-source-constructors.sh
bash scripts/tests/test-webapp-compiler.sh   # compiled Tenant subject
go test ./scripts/tests/arc-staging/         # admission routing
```

A passing offline check is not deployed-policy or publication evidence. Flux
sources have had no entry in the production authorization ledger since
platform#6270, so a matcher change is reviewed in its pull request and by the
guard above rather than by a ledger approval.
