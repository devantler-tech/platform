# Operational scripts

Go commands and Bash scripts that check this repository or operate the
platform. They fall into three groups, and only the first is meant for a
contributor's machine.

## Checks you can run without a cluster or credentials

Run these from the repository root. Each reads only files in your checkout.
You need Go (the version in [`go.mod`](../go.mod)); the first run may download
the Go modules the commands use. All but the last take a few seconds.

| Check                                                                             | What it catches                                                        |
| --------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| [`validate-embedded-json`](validate-embedded-json/README.md)                      | Broken JSON inside a ConfigMap, which schema checks read as plain text. |
| [`validate-homepage-bookmarks`](validate-homepage-bookmarks/README.md)            | Homepage links without HTTPS or an icon, duplicate or colliding groups, and missing config files. |
| [`validate-feature-flags`](validate-feature-flags/README.md)                      | Invalid feature-flag definitions.                                      |
| [`validate-talos-kubernetes-compatibility`](validate-talos-kubernetes-compatibility/README.md) | A Kubernetes version the pinned Talos version does not support.        |
| [`guard-bash-local-initialization`](guard-bash-local-initialization/README.md)    | Bash functions that read a local variable before setting it.           |

Most scripts have a regression test: a Go test beside the command
(`go test ./scripts/<name>`) or a `test-*.sh` file in [`tests/`](tests).
[Validation](../docs/validation.md) covers the manifest checks, and
[`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml) shows which checks
CI runs for which paths.

## Tools that support other checks

- [`render-helm-chart`](render-helm-chart/README.md) builds the Helm renderer
  that the post-renderer and disruption-budget guards use.
- [`reconcile-talos-kernel-args`](reconcile-talos-kernel-args/README.md) is
  part of the offline Talos configuration check.
- [`tests/fixtures/cosign-verify-failures`](tests/fixtures/cosign-verify-failures/README.md)
  holds inputs for a diagnostic classifier.

## Operator procedures and experiments

These read or change the running platform, or use its credentials. Do not run
them as part of a contribution. They are documented for the maintainer and as a
record of how a procedure works.

- [`audit-storage-retention`](audit-storage-retention/README.md) — read-only
  audit of a snapshot captured with production read access. Its `go test`
  needs no access.
- [`validate-break-glass`](validate-break-glass/README.md) — checks an export
  of the emergency-access entry without printing it.
- [`verify-arc-app-identity`](verify-arc-app-identity/README.md) — proof run by
  a manual workflow. Its `--preflight` mode alone needs no credentials.
- [`observe-longhorn-namespace-inventory`](observe-longhorn-namespace-inventory/README.md)
  — a protected metadata observation before benchmark cleanup work. It
  authorizes no deletion.
- [`replace-wedding-standby`](replace-wedding-standby/README.md) — a
  production database repair that needs the maintainer's approval for each run.
