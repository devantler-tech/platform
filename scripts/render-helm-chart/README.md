# Offline controller Helm renderer

This module builds the unmodified Helm command with the embedded SDK's default build
metadata, using Helm 4.3.1 and Go 1.27.3. The official release binary adds version linker
flags that the controller does not set. Its `v4.3.1` capability differs from the controller's
`v4.3` capability.

The audited profile keeps the Flux distribution at exact 2.9.6 and advances only
helm-controller to the adjacent-minor 1.7.1 image and CRD pair. The other controllers
stay at source-controller 1.9.6, kustomize-controller 1.9.6 and notification-controller
1.9.4. Flux's [compatibility policy](https://fluxcd.io/flux/releases/#supported-releases)
supports this adjacent-minor controller combination.

Both provider FluxInstances replace the image with the signed multi-platform index
`ghcr.io/fluxcd/helm-controller:v1.7.1@sha256:54526cc6ed1202d71d44c310676448065664f3255ad671d9a69ac26f904e617a`.
The matching CRD operations come from released flux-operator 0.62.1's
`patch instance -v 10 -c helm-controller`, with input tests for the CRD identity and
served/stored `v2`. The production image operations share its existing HA patch.
The public controller image's
Linux amd64 manifest is
`sha256:cb69af98bf251ef50951bbd01834360977066b85abfcff99a5075c3528eedd13`.
Its extracted binary's `go version -m` identifies Go 1.27.3 and Helm 4.3.1, module checksum
`h1:UndCoLe9UPmNoL7IvWzaUXveL7T5t/ijoorEmgJdI9g=`, without Helm version linker flags.

The guard builds with the exact Go toolchain and verifies the complete build metadata
before using this command for install and upgrade rendering. Helm owns values parsing,
dependency processing, schema validation and chart admission. The guard still rejects
historical release inputs and unaudited profiles. This is an offline profile; it does
not discover live APIs, objects or encrypted values.

`scripts/guard-controller-helm-profile.sh` binds the renderer to the exact base,
guarded signed image override and generated CRD operations. The post-render guard
checks the effective FluxInstance in each rendered cluster, so stock Flux 2.9.6
(helm-controller 1.6.5 / Helm 4.2.4) cannot use the 4.3.1 rendering verdict.
`scripts/tests/test-controller-helm-profile.sh` composes the committed instances
with released operator 0.62.1 and its immutable manifest artifact, checks the full
upstream 1.7.1 CRD spec, and retains replicas and the identity/RBAC boundary.
WebApp schema fixtures use the unchanged 1.9.6 source/kustomize controllers.

Keeping distribution 2.9.6 allows the operator's compatibility gate to return to
the previous 2.9.x profile. A full 2.10 distribution upgrade would refuse that
minor downgrade. Returning to the previous Git profile removes the image override
and CRD patch while retaining the production HA operations. Static native builds
reproduce that previous profile; live reconciliation after returning from Helm
4.3.1 to 4.2.4 remains unobserved.
