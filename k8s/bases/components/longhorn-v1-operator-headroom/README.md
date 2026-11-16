# Optional operator rollout headroom

This component is off by default. A separate reviewed change can select it in
the Hetzner controllers overlay after native rendering and fallback-capacity
checks pass. The existing runtime-headroom component does not select it.

When selected, the Flux operator retains its ordinary Linux placement except
for worker 3, which needs room for an additional storage manager during the
Longhorn maintenance rollout. Its replicas, resources, security, volumes and
rolling strategy remain with the owning release. The existing rolling surge
keeps the serving operator available while its replacement schedules.

Before activation, qualify admitted and allocated requests and resident memory
on eligible fallback hosts, including the surge. After rollout, require the
current Deployment generation to have one updated, Ready and available operator
away from worker 3, its old ReplicaSet retired, and healthy reconciliation. A
fresh protected storage census must show complete declared overlap FIT before
the maintenance runtime is selected. Configuration rendering alone proves none
of those live outcomes.

Remove the selection through a reviewed change after the migration runtime and
backup checks pass. Include this cleanup and fresh fallback-capacity and
availability checks before any eligible host maintenance, including the first
permanent-worker preparation. Removing the component restores ordinary placement.
