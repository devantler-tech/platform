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
publication guards against constrained constructors and mutated graph contexts.

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

## Consumer discovery conservation

CI also checks that the publish-workflow signing-revision report discovers
exactly the consumers declared by the production overlay and its Flux layers:

```bash
bash scripts/guard-consumer-discovery-conservation.sh
bash scripts/tests/test-guard-consumer-discovery-conservation.sh
```

This check uses `yq`, `realpath` and `kubectl kustomize` without a cluster or
credentials. The approval generator records current publications and pins with
their full publisher identity in
`scripts/publish-workflow-approved-revisions.identities.tsv`. The original
approved set keeps legacy compatibility evidence when a consumer moves to the
canonical publisher. A canonical observation must match one of that consumer's
fixed reviewed approvals; daily regeneration cannot approve a new canonical
revision.

The legacy release candidate is historical evidence in
`scripts/frozen-legacy-publisher-release.tsv`, not a live release lookup. The
reviewed record binds the final stable release to one commit and both publisher
source blobs. Regeneration validates the complete record and refuses malformed,
missing or inconsistent evidence without querying the retired repository. The
obsolete legacy-release watcher is removed; the scheduled approval regeneration
still observes real consumer publications and current pins. This does not approve
any maintained revision or waive its consumer-specific canonical approval.

```bash
bash scripts/tests/test-frozen-legacy-publisher-release.sh
```

The framework supports `publish-manifests` and `publish-app`, but an approval
must match the registered consumer's publishing workflow. Family support alone
grants no signer authority: a separately reviewed record must name the consumer,
maintained repository and exact commit before the writer can render that identity.
Application publisher activation still requires source review, tenant pin-guard
preparation, deployment and actual publication verification. Do not hand-edit the
generated matchers or infer an approval from a successful parser test.

Application acceptance spans three generic verification paths: admission, Talos
node pulls and the KRO tenant source. They accept only the explicitly reviewed
maintained revisions, not that repository's entire history, and no longer accept
the deleted legacy publisher repository at any commit (#6264). The committed-profile
controls exercise those paths and the three registered application matchers with
accepted and rejected identities. A passing offline predicate check is not
deployed-policy or publication evidence.

Each of those paths, and every registered consumer except `.github`, also accepts a
product's own release workflow on a release tag (#6265), as one further
alternative written last inside the same anchored subject. The image rules accept
any first-party repository starting with a letter or digit; a `Tenant` source
accepts only `devantler-tech/<spec.name>`, regex-escaped; a consumer accepts only
its current repository, which the writer renders from a fixed per-consumer rule,
never from an observation. The workflow file sits directly in `.github/workflows/`
and the tag follows the strict semver grammar of the KSail `cd.yaml` rule. The
offline controls exercise own, lookalike and other repositories, branches,
non-release and suffixed tags and nested paths at every layer:

```bash
bash scripts/tests/test-publish-workflow-canonical-approvals.sh
bash scripts/tests/test-tenant-source-constructors.sh
bash scripts/tests/test-webapp-compiler.sh   # compiled Tenant subject
go test ./scripts/tests/arc-staging/         # admission routing
```

The independent shared-publisher pin guard accepts the same strict two-family
form for both workflows. In the three generic verifiers it accepts only the
canonical-only form, optionally grouped with that verifier's fixed own-release
alternative, so the deleted legacy publisher's commit-pinned identities stay
refused there; a registered consumer keeps only the exact legacy commits its
generated approvals name. Each per-tenant source accepts only its own
repository's release workflow: in a consumer matcher it must be one literal name
other than `actions` and `.github`, and which name is the approved-revisions
guard's question. The image and Talos rules accept any first-party repository's
release workflow on a release tag except `.github`, which hosts reusable
workflows other repositories call. A repository named `actions` would be a
first-party repository like any other, so its release tag passes them; the
per-tenant binding is the tenant's signed manifest source, which pins its image
digests.
Flux sources have had no entry in the production authorization ledger since
platform#6270, so a matcher change is reviewed in its pull request and by the
guards above rather than by a ledger approval.

To move a consumer's canonical publisher to a newer commit, review the workflow
at that commit, add it as a second row for the consumer in
`scripts/publish-workflow-canonical-approvals.tsv` below the current one, and run
`bash scripts/write-publish-workflow-matchers.sh`. The matcher then accepts both
commits, so the release signed at the old commit and the first one signed at the
new commit both verify. Deploy that before the consumer moves its pin. Once the
consumer has published from the new commit, remove the first row and run the
writer again. A consumer holds at most two rows.

The overlay and all production roots render from an isolated
snapshot of KSail's published `k8s` files: YAML/YML/JSON files selected by
case-insensitive extension, without ignore-file filtering. Directory links are
not traversed and other inputs are omitted; selected file links are read only
after their targets are bounded to the source tree and staged as regular bytes.
Reachable resources, bases, components and file-loader inputs (patches,
transformer configurations, CRDs, generators, replacements and OpenAPI
definitions) must resolve within that artifact. Absolute, nonlocal, omitted or
uninspectable inputs are unknown and fail before rendering. A separate
declared-path count must match the complete extracted path census; successful
but empty or truncated reader output cannot authorize a build. Inline patches,
single-document transformer configs and generator literals remain supported.
Inline generator and transformer strings must have exactly one document
according to the YAML parser; decoded bytes must match the original serialized
string, and multiple documents or incomplete parser receipts are unknown.
Separators inside literal data remain supported. Contained relative manifest
bases and selected file links remain supported; unused Kustomizations are not
traversed. Empty regular selected files and selected directory links prevent
publication and fail the check. Empty selected link targets are conservatively
refused as unknown. It compares literal OCIRepository names and namespaces,
exact URLs, effective refs and signer subjects. An overlay rename is a
divergence even when it keeps the same artifact and revision. Identical consumer
rows repeated across roots are deduplicated. All rendered top-level Flux OCI
declarations are checked for conflicting contracts that could overwrite an
attributed consumer, including unsigned sources; an absent namespace cannot
establish disjointness. Such conflicts are unknown and fail the check. Unrelated
unsigned identities remain outside the report. Guard-only identity rows preserve
the report's four-column `--list-consumers` output and artifact-based revision
tables. It binds the layers to the platform artifact configured in
`ksail.prod.yaml` and the generated `OCIRepository/flux-system/flux-system`
source. Agreeing references to a different source, or declarations that redirect
that source, are unknown and fail the check. FluxInstance source patches are
supported only when their operations preserve the source identity and artifact
(verification and ref fields). Explicit source content selectors (`ignore` and
`layerSelector`) are refused because the same URL can produce a different tree.
A production root's non-empty `targetNamespace` is also an unseen Flux transform
and is refused.

Admission mutations that can match consumer or production-root kinds are
refused, as are unbounded kind matches and unevaluated CEL mutations. Literal
mutations of unrelated kinds remain supported. Mapping-backed controller
templates cannot add a Kustomization on the platform source: its additional path
would be missing from the rendered set. A declared `semverFilter` also fails
closed because the signing-revision resolver does not evaluate Flux's tag
filtering. Only objects in the Flux source API group count as OCI consumers, and
an attributed source must have no suspension field or a literal boolean false.
Generated sources, roots and controller carriers are refused; literal unrelated
generated kinds remain supported. Consumer-producing kro instances are counted
in mapping-backed controller carriers as well as top-level documents.
Substituted structural or consumer contract keys are unknown, just like
substituted contract values. Nested source references must also be literal,
including an inherited namespace. An OCI-producing kro definition may retain
dormant references only while its complete literal schema GVK has zero matching
instances across every production render. Native mutating webhooks that can
target sources, roots or their policy/controller carriers are refused because
the static build cannot evaluate their callbacks; literal unrelated resource
groups and Pod-only rules remain supported. The same boundary includes policies
and controller carriers that can create or rewrite those resources later. Nested
ResourceSet string templates and substituted carrier keys are unknown. Direct
and foreach clone lists are checked against every consumer-producing kro schema
across all production roots; only literal foreign group/version selectors can
rule out a schema. CEL generation remains unevaluated. Mapping-backed
[ResourceSet resources](https://fluxoperator.dev/docs/crd/resourceset/#resources-configuration)
and Kyverno generation branches must name literal object types before that
comparison. Controller template expressions in nested source references are also
unknown; workload metadata references remain supported when the generated object
type is literal. Nested FluxInstance templates also create sources and roots
outside the static render and are refused. Dormant OCI-producing kro definitions
retain the complete schema and zero-instance requirement across every root.

Flux
[substitutes the final YAML after the build](https://fluxcd.io/flux/components/kustomize/kustomizations/#post-build-variable-substitution),
so a variable in an object or template kind or API version can hide an OCI
consumer from literal discovery. The check refuses those object types, top-level
source name/namespace variables, and consumer URL, ref or signer-subject
variables; ordinary variables in workload fields, ConfigMap data and registry
credentials remain supported. A same-named source with an omitted or empty
namespace is checked as a possible platform source; an explicit different tenant
namespace keeps its own source contract. A failed reader or render is unknown
even when it produced partial output. Tenant artifacts and Helm chart output
remain outside this repository's static comparison.
