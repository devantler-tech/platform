# OCI chart source readiness

The OCI charts for Flux Operator, KRO, Homepage, Origin CA Issuer and KSail Operator
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

KSail Operator uses the same source shape and, because its chart is first-party,
also verifies the signer: only a chart signed by KSail's `cd.yaml` on a release
version is accepted. Charts before 7.202.16 carry no signature and cannot be
installed from this source. Its version is still proposed from KSail's GitHub
releases, read from the source's version, so a release that built no operator
image is never proposed.

`go test ./scripts/tests/oci-chart-sources` checks source/release bindings, fixed
tags, chart layers, update discovery and negative controls. CI runs this alongside
the existing artifact-verification and render checks. After a deployment, verify
that the migrated sources are Ready and their releases remain healthy. Historical
Coroot errors age out normally; their presence does not justify erasing logs.
