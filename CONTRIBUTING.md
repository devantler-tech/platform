# Contributing

Thanks for helping improve the platform. This repository configures a live
business and personal platform, so changes should be small, reviewable, and
safe to deploy. You do not need production credentials or a running cluster to
make most contributions.

If you want your own platform rather than a change to this one, start with
[platform-template](https://github.com/devantler-tech/platform-template).

## Choose a change

- Check the [existing issues](https://github.com/devantler-tech/platform/issues)
  before starting. Discuss a non-trivial change in an issue first so its scope
  and intended outcome are clear.
- For a bug, include the expected result, observed result, and steps to
  reproduce it. Remove credentials and sensitive details from examples.
- Report vulnerabilities privately through the [security policy](SECURITY.md),
  not a public issue.

## A first contribution

A good first change is one you can check completely on your own machine. You
need Git, [Go](https://go.dev/dl/) (the version in [`go.mod`](go.mod)) and
`kubectl`. You do not need a cluster, an account on any cloud, or any of this
repository's secrets.

One example is adding a link to the platform's start page:

1. Fork the repository, clone your fork and create a branch.
2. In [`k8s/bases/apps/homepage/config-map.yaml`](k8s/bases/apps/homepage/config-map.yaml),
   add an entry under an existing group in the `bookmarks.yaml` block, copying
   the shape of its neighbours: a name, an `icon` and an `https://` `href`.
3. From the repository root, run the two checks that cover that file:

   ```bash
   bash scripts/validate-homepage-bookmarks.sh
   kubectl kustomize k8s/bases/apps/homepage/ > /dev/null
   ```

   The first prints how many bookmarks it checked and fails on a missing icon,
   a link without HTTPS or a duplicate name. The second confirms the folder
   still builds. The first run may download Go modules; after that both take a
   few seconds.
4. Open a draft pull request as described under
   [Propose the change](#propose-the-change).

None of these commands contacts a cluster or uses a credential. They leave
your checkout unchanged; Go keeps downloaded modules in its own cache. The change itself reaches the running start page only after review,
when the pull request merges.

Other good first changes are corrections to a guide or a directory README. The
[scripts index](scripts/README.md) lists the checks that are safe to run
locally and names the operator procedures that are not.

## Work locally

Fork the repository and create a branch for one change. If you work from the
portfolio monorepo, use an isolated worktree; do not edit its shared submodule
checkout.

Read the [architecture guide](docs/architecture.md) to find the right layer:

- `k8s/bases/` holds shared components.
- `k8s/providers/` holds provider-specific differences.
- `k8s/clusters/` wires each environment together.
- `scripts/` holds operational Go commands and Bash scripts.

Keep manifests to one resource per file, follow nearby naming and overlay
patterns, and preserve Flux dependency ordering. Update generated bundles
through their updater rather than editing the generated files. Detailed
technical conventions live in [AGENTS.md](AGENTS.md).

Never commit plaintext secrets or production credentials. The committed
encrypted secrets belong to this instance; they are not usable in your fork.
Running your own test cluster is optional and needs your own encrypted values;
follow [local development](docs/local-development.md) for prerequisites, setup,
opt-in components, and cleanup. The Docker environment is a thin test-bed, not
a production replica. Do not test by changing the maintained production cluster.

## Validation

Run checks from the repository root, choosing those affected by your change:

- **Manifests:** install [KSail](https://github.com/devantler-tech/ksail) and
  validate both environments. These commands do not start a cluster:

  ```bash
  ksail workload validate
  ksail --config ksail.prod.yaml workload validate
  ```

- **Go or Bash tooling:** add a regression test for the changed behaviour and
  run the affected package or script tests. Consult the
  [validation contract](AGENTS.md#validation) and the path filters in
  [CI](.github/workflows/ci.yaml) for the relevant commands.
- **Documentation:** check formatting, relative links, and anchors. Verify
  commands and factual claims against the configuration or cited sources.

The [validation guide](docs/validation.md) explains the manifest checks and
consumer-discovery guard in more detail. Static checks prove configuration is
well-formed; they do not prove a production rollout succeeded. For a runtime
change, describe how to verify its effect safely before and after deployment.

## Propose the change

Open a draft pull request linked to the issue. Use a Conventional Commit title,
such as `fix: preserve backup verification on retry` or
`docs: clarify local setup`, and follow the pull request template's **Why** and
**What** sections in plain English.

Sign your commits, stage only the files you changed, and include the checks you
actually ran and any limitations in a PR comment. Keep operational instructions
and longer explanations in the relevant guide rather than expanding the README.

Address review findings and wait for required CI and review before the PR is
marked ready. Maintainers use the merge queue; for Kubernetes changes, its
production deployment gate must also pass before the change merges. CI on a
pull request validates and scans configuration but does not boot a test cluster.
