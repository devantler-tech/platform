# V1 runtime rollout headroom

The Hetzner controllers overlay enables this temporary placement for the V1 runtime
rollout in #6073: Velero's server uses the temporary V2 migration pool, Hetzner CSI
uses the three existing migration workers, and Reloader and the Flux Helm
controller use the existing control-plane nodes. Only Reloader and Helm receive
a narrow control-plane NoSchedule toleration.
CSI and Reloader require separate hosts across revisions so their rolling surges
cannot concentrate controller requests. Their replicas, resource requests,
disruption budgets, security settings and other spreading rules remain
configured by their owning releases. Node agents
keep their existing placement. No server or storage capacity is added.
The Helm controller's placement lives in the production FluxInstance's existing
controller patch, because the FluxInstance reconciles in its own layer.

Before rollout, verify the three temporary workers and three control-plane nodes are
Ready, have no pressure and have both request and resident-memory headroom for
their controllers. Include the current admitted allocations, not only chart
defaults and account for overlapping old and new replicas during rollout. Verify
the migration pools retain their fixed capacity floors and that Helm-controller,
Reloader and CSI replicas can spread across separate nodes. Wait for
active backup and restore operations to finish before moving Velero; its
single-server Recreate rollout briefly pauses backup orchestration.

After rollout, require every affected Deployment to reach its current desired
replicas, each disruption budget to select its workload, and normal volume
attachment to remain healthy. Re-run the complete native runtime census.
Placement itself proves neither overlap capacity nor adoption of the approved
instance-manager image. A remaining HOLD still blocks adoption.

Remove these temporary placements, including the FluxInstance controller patch,
through a reviewed change after the migration's
runtime and backup checks pass, before rebuilding or retiring any temporary
CSI or Velero target. Keep that cleanup within #6073 and verify the
ordinary worker placement has sufficient headroom before returning controllers
there. Returning to the preceding Git configuration restores their ordinary
placement.
