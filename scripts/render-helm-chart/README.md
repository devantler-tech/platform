# Offline controller Helm renderer

This module builds the unmodified Helm command with the embedded SDK's default build
metadata, using Helm 4.2.4 and Go 1.26.8. The official release binary adds version linker
flags that the controller does not set. Its `v4.2.4` capability differs from the controller's
`v4.2` capability.

The audited profile is Flux 2.9.6 / helm-controller 1.6.5. The public controller image's
Linux amd64 manifest is
`sha256:777f34e82f901d247cab555e67cdb278a8da509cca7668f5b951bbf15b85f4ad`.
Its extracted binary's `go version -m` identifies Go 1.26.8 and Helm 4.2.4, module checksum
`h1:qIysMI0JpTC4WXf3AQ99V6rZGT0+gO0Ww8IOnnUnaZk=`, without Helm version linker flags.

The guard builds with the exact Go toolchain and verifies the complete build metadata
before using this command for install and upgrade rendering. Helm owns values parsing,
dependency processing, schema validation and chart admission. The guard still rejects
historical release inputs and unaudited profiles. This is an offline profile; it does
not discover live APIs, objects or encrypted values.
