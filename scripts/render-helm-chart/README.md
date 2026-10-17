# Offline controller Helm renderer

This module builds the unmodified Helm command with the embedded SDK's default build
metadata, using Helm 4.2.0 and Go 1.26.3. The official release binary adds version linker
flags that the controller does not set. Its `v4.2.0` capability differs from the controller's
`v4.2` capability.

The audited profile is Flux 2.8.8 / helm-controller 1.5.5. The public controller image's
Linux amd64 manifest is
`sha256:fe761c0f60a3cff5e0c298ea9da1babf5be44251e68b48c9731a5de6609dd93f`.
Its extracted binary's `go version -m` identifies Go 1.26.3 and Helm 4.2.0, module checksum
`h1:J+0TmTtPK2NuS6z9Z2WOcIX0nGGJylokEZLt0fi0X4U=`, without Helm version linker flags.

The guard builds with the exact Go toolchain and verifies the complete build metadata
before using this command for install and upgrade rendering. Helm owns values parsing,
dependency processing, schema validation and chart admission. The guard still rejects
historical release inputs and unaudited profiles. This is an offline profile; it does
not discover live APIs, objects or encrypted values.
