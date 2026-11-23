# ADR: Adopt API-only KHoist

- **Status:** Accepted for the v9.0.5 adoption.
- **Tracks:** [Platform #4890](https://github.com/devantler-tech/platform/issues/4890),
  [Platform #4895](https://github.com/devantler-tech/platform/issues/4895),
  [Platform #4874](https://github.com/devantler-tech/platform/issues/4874) and
  [KHoist #392](https://github.com/devantler-tech/khoist/issues/392).
- **Source decisions:** KHoist
  [ADR 0023](https://github.com/devantler-tech/khoist/blob/v9.0.5/docs/adr/0023-api-only-product-integration.md)
  and [ADR 0024](https://github.com/devantler-tech/khoist/blob/v9.0.5/docs/adr/0024-retire-legacy-ui-declaration-within-v1alpha1.md).

Data products integrate through published APIs. Consuming applications own their
interfaces, so the platform no longer runs a host for embedded product pages.

Platform moves from the historical `data-product-controller` 2.9.1 artifacts to
KHoist 9.0.5 in one revision:

- image `ghcr.io/devantler-tech/khoist@sha256:e485a0e82df734e6fb5ec8b8f3ca923748b82b9cab81be86a0c69e3e116cdfbf`,
  signed by the reviewed shared `publish-app` workflow from the v9.0.5 tag;
- chart `oci://ghcr.io/devantler-tech/charts/khoist@sha256:ccb64ccf46b96802a37ac1f362c543e70cddb669025df62e7f59dfc62d814fd4`,
  signed by KHoist's own `publish-chart` workflow from a release tag.

as they are. The chart render differs only by artifact names and the removed
presentation settings and `spec.ui` declarations. The release upgrades CRDs through
`crds: CreateReplace`, so the admission rule that rejects new `spec.ui` values applies
before the workloads change; removing the field from a stored product stays allowed.
The release sets `chartNameChangeStrategy: InPlaceUpdate`: its default would uninstall the
release because the chart name changes, deleting the workloads and registrations.

`/ui-kit` no longer carries product sessions, so `product-ui.${domain}` is retired.
The offline report tool keeps its separate release gate and moves to the dedicated
`/publisher-review` application at `publisher-review.${domain}`. It remains public,
because the browser reads a local report without uploading it, and has no outbound
network access, token or data-source credential.

The KSail release page stays a directly served interface of the sample workload.
`demoProduct.ksailReleases.enabled` only lists it in the registry. To withdraw the page
itself, roll back; delisting does not remove it. This settles Platform #4874.

The rollout verifier takes the exact image and chart from this checkout and refuses
caller digests or a live release that differ. It replaces the retired parts of the
receipt with requirements that its two named products do not declare `spec.ui` and that
no serving workload carries the retired settings. Products elsewhere are counted, not
refused. It also requires the in-place chart rename.

## Amendment: KHoist 9.0.15 (2026-11-23)

The platform moves to KHoist 9.0.15, the newest release, signed by KHoist itself (monorepo ADR 0017):

- **Image:** `ghcr.io/devantler-tech/khoist@sha256:e19eebc960af47442b583ff547fcf1ecef80cc574c1825599485fc93669a7764`, signed by `devantler-tech/khoist/.github/workflows/cd.yaml@refs/tags/v9.0.15`.
- **Chart:** `oci://ghcr.io/devantler-tech/charts/khoist@sha256:56bcec6934e9ae1564928de6b0e145a18c7948c3566de5abd676782506f6037f`, signed by `publish-chart.yaml@refs/tags/v9.0.15` as before.

The controller and its probes no longer need the shared `publish-app` publisher. The generic image rules already accept a first-party repository's own release workflow on a release tag.

9.0.13 to 9.0.15 contain:
- KHoist signing its own releases;
- a zlib security rebuild of the PostgreSQL AGE image, which the platform does not run;
- a Go dependency bump, chart unit tests and template-sync changes.

The chart's templates, values and CRDs are unchanged, so the decision above and the CRD upgrade path still apply. The chart still follows a pinned digest; following KHoist's release tags is Platform #6428.

To roll back this amendment, revert its change. The generic image rules still accept the v19.0.5 `publish-app` revision that signed 9.0.12, so the 9.0.12 image verifies again.

## Amendment: KHoist 9.0.12 (2026-11-13)

The platform moves to KHoist 9.0.12:

- **Image:** `ghcr.io/devantler-tech/khoist@sha256:fb97d7bef59c9898373f65acb90a001313806c1112bef295b464c3d161563056`.
- **Chart:** `oci://ghcr.io/devantler-tech/charts/khoist@sha256:806cbdd4513f8ef9593f5426f5c609669651b2a4fee5970af2b3bc0519be0341`.

The image is signed by the reviewed v19.0.5 `publish-app` revision (`af042c54…`), not by v8.0.0. That frees v8.0.0's slot in the generic image rules for a publisher whose signature bundle every node can read (Platform #6226, #6227). The chart keeps KHoist's own `publish-chart` signer.

9.0.6 to 9.0.12 contain:
- a Go 1.27.4 security rebuild;
- dependency bumps, including OpenTelemetry 1.47.0;
- test and CI fixes.

The CRD changes only its `controller-gen` version annotation, and its schema is unchanged. So the decision above and the CRD upgrade path still apply.

To roll back this amendment, revert its change. That restores the 9.0.5 image, chart and probe pins. The data-product-controller approvals and the documentation source keep accepting v8.0.0 until it is retired (Platform #6228), so the 9.0.5 artifacts still verify. Do not apply the rollback of the 9.0.5 adoption below for this transition: it returns to 2.9.1, `ui-kit` and the previous verifier.

## Rollback of the 9.0.5 adoption

Restore the 2.9.1 image, chart source and signing identity, the `ui-kit` resources and
the previous verifier in one revert. Flux re-applies the 2.9.1 CRD through
`CreateReplace`, which restores `spec.ui` writes and the sample's declaration. Stored
`v1alpha1` objects need no conversion.

## Not established by this decision

The deployment receipt does not prove browser behaviour or a separate consumer. Those
are checked after the deploy and recorded on Platform #4890. The receipt's product counts
supply the zero-UI inventory that ADR 0024 requires before KHoist removes the field.
