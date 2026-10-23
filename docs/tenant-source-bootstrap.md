# Bootstrap a signed tenant source

Keep an application's `DNSEndpoint` declaration beside its deployable configuration.
The platform owns tenant registration, the shared ExternalDNS writer and the admission
policy that limits which records the tenant may declare. A documentation-only tenant
reconciles its documentation subdirectory with a service account permitted to manage
only the required namespaced DNS objects.

The normal approved-revision generator reads a source that Flux has already fetched
and verified. A first consumer cannot supply that observation. Do not add a guessed
row to `publish-workflow-approved-revisions.tsv` or loosen the signature matcher.

## Verify the first publication

Prepare an immutable `OCIRepository` in the platform checkout. For the documentation
tenant, its identity is `data-product-controller/khoist-documentation`, its URL is
`oci://ghcr.io/devantler-tech/data-product-controller/manifests`, and its ref contains
only the release's `sha256:` manifest digest. Do not register the child Flux
Kustomization yet: the waiting apps layer also health-checks suspended children,
whose unobserved generation would stall that layer.
Include the normal Cosign issuer and shared `publish-app` identity block so the
existing matcher writer can discover it. That writer replaces its revision set
with the derived approval before registration; bootstrap is no exemption from
source signature verification.

Replace the context and release placeholders with the approved target and published
release being onboarded.

```sh
PUBLISH_KUBE_CONTEXT='<approved-context>' bash scripts/bootstrap-tenant-source.sh \
  --source-file k8s/providers/hetzner/apps/khoist-documentation/oci-repository.yaml \
  --tag vX.Y.Z
```

This explicit first-consumer operation requires a complete cluster source listing,
an absent or exact unfetched source, a successful publication run at the tag's commit,
and an immutable Cosign signature bound to the expected publisher, caller repository,
tag and source commit. It rereads the tag and source declaration before creating
`scripts/publish-workflow-bootstrap-publications.json`. A failed or ambiguous read
writes no approval. Existing applied evidence must use ordinary regeneration.

The companion's `published-bootstrap` entry supplies the first signature revision
set. The ordinary eight-column table continues to mean observed Flux evidence. The
matcher writer and guard read both formats without combining historical publication
revisions with an existing ordinary row.

Register the new consumer and artifact in the report's explicit identity floors,
include its source in the regeneration workflow's consumer-read token and output
paths, and run the existing matcher writer and guard. Review this change normally.
Only the documentation-consumer path creates a bootstrap approval. Another product
needs a reviewed extension rather than a free-form identity argument.

## Verify business-site manifest evidence without approval

The explicit business-site profile verifies one maintained-publisher manifest
publication. It reads public GitHub metadata and the immutable registry signature;
it does not use a Kubernetes context or change cluster state. With `gh`, `jq`,
`yq` v4 and Cosign available, supply the tag, source and manifest digest from the
separately verified paired-release evidence recorded on the consumer's issue:

```sh
bash scripts/bootstrap-tenant-source.sh --consumer business-site --verify-only \
  --tag v1.0.2 \
  --source-sha e5eb256dcb5dbf3857257e9072937ab95f1ea6c3 \
  --digest sha256:6adcb1794ebdf5de4e1b03f758d44d6f9061eee43b863b1b1f4955a1e1d2615e
```

`--consumer` must be first. This profile requires `--verify-only` and refuses
output paths, other consumers, duplicate arguments and deployment options. It
accepts only the business site's source-owned runtime caller, a successful
`release` run, a published stable release and the exact reviewed maintained
publisher identity. The signature certificate must also name the `release` event;
another event at the same tag and source does not qualify. Signed publication
claims bind the manifest to that exact run and attempt, repository, source, tag,
version and publisher. The release must use a direct commit tag. Incomplete or failed
reads emit no receipt, including a failed verifier that printed plausible output.
The tag, release and original current run attempt are rebound after verification.

Success prints one `manifest-publication-evidence` JSON receipt with
`deploymentAuthorized: false`. That receipt is deliberately not readable as the
legacy bootstrap-approval schema. It writes no approval tables, matchers or
registration. It does not independently verify the paired image, all provenance
claims, a Flux binding or a deployed tenant. Keep the source's independent paired
release verification alongside it. Platform #5247 retains private-input ownership,
typed source/reconciliation ordering, narrow routing approval and actual runtime
acceptance. Pages and the private portal remain unchanged.

## Observe before activating reconciliation

After the reviewed source is deployed, confirm `Ready=True`, `SourceVerified=True`
and a current `observedGeneration`. Its `status.artifact.revision` must equal the
declared manifest digest. `status.artifact.digest` is a different identity: Flux's
cached archive hash. It cannot prove which registry artifact was fetched.

Run the ordinary generator, matcher writer and guard. The generator records the real
observation and changes `published-bootstrap` to `published-binding` in the same
generated change. Thereafter only the ordinary row supplies approved revisions. A
missing ordinary row cannot revive the retired bootstrap approval. The retained
binding attributes this exact digest; changing the source digest needs a newly
verified publication binding before the source can advance.

Before activating `path: ./documentation`, verify that the signed release actually
contains that directory and the product's DNS declaration. A valid older release
without the directory proves publication only. Deploy and verify the DNS admission
boundary before adding the CRD source to ExternalDNS, then reconcile the narrow
tenant and check ExternalDNS ownership, public DNS and GitHub Pages HTTPS separately.

The signing-revision report can reverify a bound digest and compare it with the
consumer's current default-branch publisher. Its output explicitly remains
publication and pin evidence; it is not a claim that the tenant was applied.
