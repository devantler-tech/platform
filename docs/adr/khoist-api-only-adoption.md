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

## Amendment: KHoist 9.0.12 (2026-11-13)

The platform moves to KHoist 9.0.12:

- **Image:** `ghcr.io/devantler-tech/khoist@sha256:fb97d7bef59c9898373f65acb90a001313806c1112bef295b464c3d161563056`.
- **Chart:** `oci://ghcr.io/devantler-tech/charts/khoist@sha256:806cbdd4513f8ef9593f5426f5c609669651b2a4fee5970af2b3bc0519be0341`.

The image is signed by the reviewed v19.0.5 `publish-app` revision (`af042c54…`), not by v8.0.0. That frees v8.0.0's slot in the generic image rules for a publisher whose signature bundle every node can read (Platform #6226, #6227). The chart keeps KHoist's own `publish-chart` signer.

9.0.6 to 9.0.12 contain:
- a Go 1.27.4 security rebuild;
- dependency bumps, including OpenTelemetry 1.47.0;
- test and CI fixes.

The CRD changes only its `controller-gen` version annotation, and its schema is unchanged. So the decision above, the CRD upgrade path and the rollback below still apply. A rollback now restores 9.0.5's image and chart. Its v8.0.0 signer stays accepted until v8.0.0 is retired.

## Rollback

Restore the 2.9.1 image, chart source and signing identity, the `ui-kit` resources and
the previous verifier in one revert. Flux re-applies the 2.9.1 CRD through
`CreateReplace`, which restores `spec.ui` writes and the sample's declaration. Stored
`v1alpha1` objects need no conversion.

## Not established by this decision

The deployment receipt does not prove browser behaviour or a separate consumer. Those
are checked after the deploy and recorded on Platform #4890. The receipt's product counts
supply the zero-UI inventory that ADR 0024 requires before KHoist removes the field.
