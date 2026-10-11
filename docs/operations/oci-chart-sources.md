# OCI chart source readiness

The third-party OCI charts for Flux Operator, KRO, Homepage and Origin CA Issuer
use `OCIRepository` sources and `HelmRelease.spec.chartRef`. Each source names the
complete chart URL, an exact version tag, a reconciliation interval, and the Helm
chart layer with `operation: copy`. Versions live in the sources rather than the
releases; values, post-renderers and release identities remain with the releases.

Unlike an OCI-type `HelmRepository`, an `OCIRepository` fetches an artifact and
reports readiness. This avoids Coroot trying to read a readiness condition from
a data-only HelmRepository. Chart fetch failures remain observable through the
source and HelmRelease; no monitoring threshold, metric or log check is disabled.

The Flux Operator's dedicated Renovate manager reads the source's version tag.
The other migrated charts retain built-in Flux dependency discovery. The existing
release-age policy is unchanged.

KSail Operator remains on its existing HelmRepository source. Its current chart
has no published signature, while the platform requires first-party
OCIRepository artifacts to verify a signing identity. Migrating it requires a
signed chart and verified adoption; adding an exemption is not the repair.
That remaining source can still produce the Coroot missing-conditions message.

`go test ./scripts/tests/oci-chart-sources` checks source/release bindings, fixed
tags, chart layers, update discovery and negative controls. CI runs this alongside
the existing artifact-verification and render checks. After a deployment, verify
that the migrated sources are Ready and their releases remain healthy. Historical
Coroot errors age out normally; their presence does not justify erasing logs.
