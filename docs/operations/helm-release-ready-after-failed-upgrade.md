# A HelmRelease can report Ready after its upgrade failed

`Ready=True` on a HelmRelease is not proof that its latest configuration is what runs. When an
upgrade is triggered **only** by a change to `.spec.postRenderers` or `.spec.commonMetadata` and it
fails, helm-controller reports the failure once and then goes back to `Ready=True`, describing the
previous release. The cluster keeps running the output of the old configuration (#4376).

This happened on 2026-09-04 (#3577): the flux-operator release failed in the post-render step and
reported `Ready=True` / `UpgradeSucceeded` for its five-day-old release, with `observedGeneration`
equal to `generation`, while every later reconcile logged that the release was in sync.

## Why it happens

It is helm-controller behaviour, read from the released source of v1.5.5 (the version production
runs) and unchanged in v1.6.5 and on its `main` branch on 2026-10-07:

- The controller decides a release needs an upgrade by comparing the chart, the values and a digest
  of the post-renderers with what it last observed.
- After **every** release action, failed ones included, it records the current post-renderer and
  common-metadata digests as observed.
- So after the failed attempt the digests match, the chart and values still match the stored
  release, and the next reconcile finds nothing to do and reports the old release as healthy.

## What tells you

| Signal | Lasts | Where |
|---|---|---|
| The Flux reconciliation alert | One message, at the moment of the failure | Slack, from [`alert.yaml`](../../k8s/providers/hetzner/infrastructure/flux-notifications/alert.yaml); it carried the full error text on 2026-09-04 |
| A Warning `UpgradeFailed` event on the release | As long as the API server keeps events, about an hour | `kubectl get events` |
| The release's own failure count | Until its chart version, values or generation next change | `status.failures` (`status.upgradeFailures` stays at zero, because the failed attempt stored no release) |
| The helm-controller log | Until the pod restarts | `flux-system/helm-controller` |

Before a change merges, the `validate-helm-post-renderers` CI job renders each changed release's
chart and applies its post-renderers (#3581, #4377). It catches a patch that cannot apply to the
rendered chart. It cannot catch a failure that depends on the cluster: a template that branches on
the cluster's APIs, a value held only in a Secret, or a difference in how the controller renders.

## Check for it

The failure count is the only trace on the object that outlives the event. A failed upgrade raises
`status.failures`. The controller resets it when the release's generation, chart version or values
change, and after a successful upgrade only when a retry strategy applies to the release.
So a release that is `Ready=True` with a count above zero had an attempt at its **current**
configuration fail. This lists them, with the two times that tell a hidden failure from one that
was retried:

```bash
kubectl get helmreleases.helm.toolkit.fluxcd.io --all-namespaces -o json | jq -r '
  "read \(.items | length) releases",
  (.items[]
    | select(.status.conditions // [] | any(.type == "Ready" and .status == "True"))
    | select((.status.failures // 0) > 0)
    | "\(.metadata.namespace)/\(.metadata.name) failures=\(.status.failures)"
      + " deployed=\(.status.history[0].lastDeployed // "unknown")"
      + " released-condition-changed=\([.status.conditions[] | select(.type == "Released") | .lastTransitionTime][0] // "unknown")")'
```

The first line states how many releases were read; a result without it read nothing. On 2026-10-07
production printed `read 46 releases` and no release.

Read a line it prints like this:

- **`released-condition-changed` is much later than `deployed`:** the release went back to
  `Released=True` without deploying anything. That is the hidden failure. On 2026-09-04 the release
  it described had been deployed five days earlier.
- **The two are within the time an upgrade takes:** a later attempt deployed. The count is left
  over because no retry strategy applies, and the release is fine.

This is a derived signal, not one the controller designed, and it has three limits:

- **It can clear while the release is still stale.** An edit that changes the spec without changing
  the chart, the values or the post-renderers (an interval or a timeout, for example) changes the
  generation, which resets the count and triggers no upgrade.
- **It cannot tell the two cases apart when the failure follows a deployment closely**, because the
  two times are then near each other either way.
- **It has never been observed on a real failure.** It follows from the source and no release
  matches it today. Reproducing the failure needs a cluster that can be written to.

A release in the first case needs the next section. A release the check does not name is not proven
healthy.

## Repair

1. Read the error: the Slack alert, or `kubectl -n flux-system logs deploy/helm-controller`
   filtered on the release name.
2. Fix the post-renderer so it applies to the chart's rendered output, and run the check in
   [Validation Scenarios](../../AGENTS.md#validation-scenarios) (item 4) locally.
3. Merge the fix. Changing the post-renderers changes their digest, so the controller attempts the
   upgrade again.
4. Confirm on the cluster that an object the post-renderer patches carries the patched value.
   `Ready=True` alone does not show it.

## Upstream

No upstream report existed on 2026-10-07. The text prepared for one is on #4376, which stays open
until the report is filed and linked there.
