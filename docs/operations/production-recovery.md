# Recover production from current main

The merge-group heal records the exact `main` checkout before validation. Manual CD and disaster recovery use the workflow dispatch revision. Recovery requires that validated revision to be the current tip of `main`, with unchanged tracked source files.

The shared deployment checks freshness before changing production. Disaster recovery checks it before creating infrastructure. The publisher repeats the check immediately before moving the production artifact's `latest` tag, after signature, attestation and matcher verification. Each check reads the main ref through the existing read-only workflow token, with a 30-second request bound. Speculative merge-group deployment retains its candidate revision and does not use the recovery check.

`PROD_RECOVERY_SOURCE=STALE` means the checkout or main tip changed. Start a fresh recovery on `main` and run all validation again. Do not switch the existing job's checkout after validation or promote its staged artifact manually. `PROD_RECOVERY_SOURCE=UNKNOWN` means freshness could not be established; repair the failed read before retrying.

The production lock, artifact evidence checks, rollout guards and orphaned-object check remain required. Freshness is checked at each boundary; the GitHub ref read and registry promotion are separate operations. A passing freshness check does not replace the exact published-revision convergence receipt or prove cleanup succeeded.

## Hold production without wedging the deploy lane

Never hold production with a required reviewer, or any other approval, on the `prod` environment.
Every production job takes the shared `prod-deploy` lock before it waits for the environment's
approval: the merge-group deploy and its heal, CD, the Validate Main convergence observer and the
recovery and migration workflows. With an approval nobody gives, the first waiting job holds the lock
indefinitely, and the lane cannot drain:

- the merge-group deploy's first step, which refuses an evicted or replaced entry so a stale run ends
  in seconds, never runs;
- rejecting the waiting deployment counts as a failed deploy that never reported stopping before
  production, so its Heal Prod job starts and waits in turn;
- runs queued behind the lock cannot be cancelled, and a normal cancellation of them has no effect;
- the merge queue times its entries out, and Renovate re-enqueues them behind the lock.

A temporary reviewer hold (#6433) wedged the lane this way from 16 to 22 November 2026.

To hold production during an incident, make speculative production entry **fail** instead of wait:

1. Set the `prod` environment's deployment branches to a custom rule that admits only `main`.
   Merge-group deploys and heals run on `gh-readonly-queue/main/…`, so they fail when the job starts,
   the queue drains, and nothing speculative reaches production.
2. Disable **Restore Stranded Prod**.

A run dispatched on `main` for the recovery itself, such as CD or a migration or expansion step, still
deploys and needs no approval. To lift the hold, restore the protected-branches deployment policy and
re-enable Restore Stranded Prod, then dispatch CD on `main` and verify the deployment.
