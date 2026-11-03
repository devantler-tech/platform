# Offline controller Helm renderer

This module builds the unmodified Helm command with the embedded SDK's default build
metadata, using Helm 4.3.1 and Go 1.27.3. The official release binary adds version linker
flags that the controller does not set. Its `v4.3.1` capability differs from the controller's
`v4.3` capability.

The audited profile is Flux 2.10.0 / helm-controller 1.7.1. The public controller image's
Linux amd64 manifest is
`sha256:cb69af98bf251ef50951bbd01834360977066b85abfcff99a5075c3528eedd13`.
Its extracted binary's `go version -m` identifies Go 1.27.3 and Helm 4.3.1, module checksum
`h1:UndCoLe9UPmNoL7IvWzaUXveL7T5t/ijoorEmgJdI9g=`, without Helm version linker flags.

The guard builds with the exact Go toolchain and verifies the complete build metadata
before using this command for install and upgrade rendering. Helm owns values parsing,
dependency processing, schema validation and chart admission. The guard still rejects
historical release inputs and unaudited profiles. This is an offline profile; it does
not discover live APIs, objects or encrypted values.
