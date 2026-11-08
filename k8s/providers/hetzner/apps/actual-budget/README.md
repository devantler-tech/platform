# Actual Budget: user namespaces in production

In production the Actual Budget pod runs in its own user namespace
(`hostUsers: false`): container IDs map to unprivileged host IDs. Actual Budget
is the one stateful workload that does this. The setting started as a pilot.
Its runtime verdict passed on
[#3604](https://github.com/devantler-tech/platform/issues/3604), and it was made
permanent in [#3605](https://github.com/devantler-tech/platform/issues/3605).

Two patches in [`../kustomization.yaml`](../kustomization.yaml) make up the
setting, and they always change together:

- [`patches/enable-user-namespaces.yaml`](patches/enable-user-namespaces.yaml)
  labels the namespace for the existing Kyverno `add-user-namespaces` rule.
- [`patches/disable-host-users.yaml`](patches/disable-host-users.yaml) appends a
  Helm post-renderer that sets `hostUsers: false` on the chart's Deployment. It
  goes last, so the base strategy, startup-probe and OIDC post-renderers keep
  their order. The chart's Deployment has no `metadata.namespace`, so the inner
  target names it exactly. Helm scopes it to the release namespace.

The sync server and the `enablebanking-seed` sidecar keep their user and group
IDs, their mounts and their shared database volume. The Deployment keeps its
single-replica `Recreate` strategy, so each rollout of this pod brings a brief
outage. The local Docker provider keeps the shared defaults.

## Validate without deploying

From the repository root, run:

```bash
bash scripts/tests/test-actual-budget-user-namespaces.sh
ksail workload validate
ksail --config ksail.prod.yaml workload validate
```

The regression needs `kubectl`, Helm, `jq` and `yq` v4. It needs no cluster and
no secrets. It checks the following:

- Production references each patch exactly once, and the pod patch targets only
  the Actual Budget HelmRelease.
- It downloads the chart version the HelmRelease pins, then renders it with
  every post-renderer, with and without the two patches. Only `hostUsers` may
  differ in the chart's workload, and the PVC and both containers must stay
  identical.
- The local overlay is unaffected, and removing both patches and adding them
  back reproduces the committed render exactly.

[`scripts/read-actual-budget-userns-maps.sh`](../../../../scripts/read-actual-budget-userns-maps.sh)
reads the live id maps of both containers. It is read-only.

## Roll back without touching storage

Remove both patch entries from `../kustomization.yaml` in one pull request.
That removes the namespace label and the appended post-renderer together. After
deployment, verify that both are gone, the replacement pod is Ready, the same
claim and volume are still bound, and budget access and the sidecar's
reconciliation work again.

Do not delete or recreate the PVC, reinstall the chart, change file ownership
recursively, or remove the namespace label on its own. None of those reverses
this setting.
