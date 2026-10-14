# ADR: Adopt the standard KSail release interface

- **Status:** Accepted for the v3.0.0 adoption.
- **Tracks:** [Platform #4874](https://github.com/devantler-tech/platform/issues/4874)
  and [Data Product Controller #373](https://github.com/devantler-tech/data-product-controller/issues/373).

Data Product Controller v3.0.0 removes the KSail release runtime gate. The release
UI, configuration and OpenAPI routes are standard behavior. Its optional Helm
listing value remains a publication choice. The source decision is
[ADR 0022](https://github.com/devantler-tech/data-product-controller/blob/v3.0.0/docs/adr/0022-standard-ksail-release-interface.md).

Platform adopts source commit `76a492622b60a5660b94a322a1613c4314410bac`, image
`sha256:581c8ca202aab83bbb6bd9fea6612dff8237204ab88b6a586cf741a3fe94a566`
and chart `sha256:0e397295a07aa3619f25371005412a3fac7de632e234b115e0fa93348fa3a96d`.
Publication and released-image smoke jobs succeeded. Independent keyless checks
bound both digests to the v3.0.0 tag and source commit, the image publication's
immutable reusable workflow and the chart's own publication workflow.

The rollout verifier derives its profile from the checked-out Helm image
repository, tag and digest and OCI chart declaration. Caller-supplied identities
and the live Helm release must agree with those declarations. Only this exact
verified tuple accepts retirement; an unknown major release or a partial tuple
is refused until its adoption is reviewed. Runtime child digests remain separate
caller-verified identities, since a multi-platform image index is not a Pod's
runtime image ID.

For this profile, the obsolete variable must be absent from the product Deployment,
current ReplicaSet and every serving Pod. Indirect `envFrom` is refused as well.
Historical owned ReplicaSets may retain the old flag and image only while scaled
to zero with no surviving Pods. Legacy releases retain the literal true check when
listed and absent/literal false check when withdrawn, including their current
ReplicaSet. Existing generation, ownership, replica, route, signature, sandbox,
appearance and network checks remain required.

Unlisting requires `demoProduct.ksailReleases.enabled: false` and removal of the
exact DataProduct registration. It does not withdraw the interface: all three
direct routes must still pass the public contract checks. Actual GitHub data,
filters, refresh, appearance and navigation require a consumer browser evaluation;
the cluster receipt does not establish upstream availability in every browser.

To withdraw the interface itself, restore the previously verified v2.9.1 image
`sha256:d82b984165bc71844c323f752f474f80a7a7f7a78972a5e1171d9f814cb9bb34`
and chart `sha256:86c93dba35f9c8904c3d31d9df06a44bd82f4ef056289eee611906c0ff8cc8bd`
together, then set the listing/gate value false. Review the complete GitOps change
and rerun the legacy rollout and browser acceptance; setting the old environment
variable on v3.0.0 cannot provide recovery. Retain Namespace and HelmRelease prune
protection and the independent UI host's publisher opt-in and withdrawal checks.
