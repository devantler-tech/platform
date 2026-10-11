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
canonical publisher. A canonical observation must match that consumer's fixed
reviewed approval; daily regeneration cannot approve a new canonical revision.

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
