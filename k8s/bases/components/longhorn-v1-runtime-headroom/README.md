# V1 runtime rollout headroom

The Hetzner controllers overlay enables this temporary placement for the V1 runtime
rollout in #6073: Velero's server uses the temporary V2 migration pool, Hetzner CSI
uses the three existing migration workers, and Reloader and the Flux Helm
controller use the existing control-plane nodes with a narrow control-plane
NoSchedule toleration. The policy-reporting UI uses workers 1 and 2 or the V2
migration worker through its production patch. Its three eligible hosts allow
two replicas and a rolling surge to remain on separate nodes. Kustomize's two controllers
use worker 1 or the migration workers; the Coroot security-progress collector
uses worker 1 or the V2 migration worker.
CSI and Reloader require separate hosts across revisions so their rolling surges
cannot concentrate controller requests. Their replicas, resource requests,
disruption budgets, security settings and other spreading rules remain
configured by their owning releases. Node agents
keep their existing placement. No server or storage capacity is added.
The Flux controllers' placements live in the production FluxInstance's existing
controller patches, because the FluxInstance reconciles in its own layer. The
security-progress and policy-reporting UI patches live in the production infrastructure layer. The collector's
existing single-writer Recreate rollout briefly pauses security-progress metrics.

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

The policy-reporting UI needs three Ready, schedulable eligible hosts for its
two replicas and rolling surge. Before cordoning, rebuilding or retiring any
of its eligible hosts, replace or remove its temporary placement through a
reviewed change. Verify the replacement hosts' request and resident-memory
budgets, complete the UI rollout and require both replicas Ready before host
maintenance begins. This prerequisite applies before the first permanent-worker
preparation, as well as before maintenance of its temporary migration worker.

Remove these temporary placements, including the FluxInstance controller patches
and the security-progress and policy-reporting UI patches,
through a reviewed change after the migration's
runtime and backup checks pass, before rebuilding or retiring any temporary
CSI or Velero target. Keep that cleanup within #6073 and verify the
ordinary worker placement has sufficient headroom before returning controllers
there. Returning to the preceding Git configuration restores their ordinary
placement.
