# V1 runtime rollout headroom

The Hetzner controllers overlay enables this temporary placement for the V1 runtime
rollout in #6073: Velero's server uses the temporary V2 migration pool; Reloader
and Hetzner CSI controllers use the existing control-plane nodes. Their explicit
affinity keeps orchestration off the V1 storage workers, and only the smaller
controllers receive a narrow control-plane NoSchedule toleration.
Their replicas, resource requests, disruption budgets, security settings and
replica-spreading rules remain configured by their owning releases. Node agents
keep their existing placement. No server or storage capacity is added.

Before rollout, verify the temporary V2 target and three control-plane nodes are
Ready, have no pressure and have both request and resident-memory headroom for
their controllers. Include the current admitted allocations, not only chart
defaults. Verify that Reloader and CSI replicas can spread across separate nodes. Wait for
active backup and restore operations to finish before moving Velero; its
single-server Recreate rollout briefly pauses backup orchestration.

After rollout, require every affected Deployment to reach its current desired
replicas, each disruption budget to select its workload, and normal volume
attachment to remain healthy. Re-run the complete native runtime census.
Placement itself proves neither overlap capacity nor adoption of the approved
instance-manager image. A remaining HOLD still blocks adoption.

Remove this temporary placement through a reviewed change after the migration's
runtime and backup checks pass, before rebuilding or retiring its temporary
Velero target. Keep that cleanup within #6073 and verify the
ordinary worker placement has sufficient headroom before returning controllers
there. Returning to the preceding Git configuration restores their ordinary
placement.
