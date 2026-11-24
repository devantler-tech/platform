# V1 runtime rollout headroom

The Hetzner controllers overlay enables this temporary placement for the V1 runtime
rollout in #6073: Velero's server uses the temporary V2 migration pool, Hetzner CSI
uses the three existing migration workers, and Reloader and the Flux Helm
controller use the existing control-plane nodes with a narrow control-plane
NoSchedule toleration. The policy-reporting UI, Hubble UI and the data product
contract probe use the three migration workers to free memory for V2 managers
on the permanent workers. Their rollouts keep both replicas available.
Kustomize's two controllers
use worker 1 or the migration workers; the Coroot security-progress collector
and the vulnerability scanner use worker 1 or the V2 migration worker. The
scanner's memory request follows its autoscaler recommendation, so a single
eligible host can leave it unschedulable once that host fills up.
CSI and Reloader require separate hosts across revisions so their rolling surges
cannot concentrate controller requests. Their replicas, resource requests,
disruption budgets, security settings and other spreading rules remain
configured by their owning releases. Node agents
keep their existing placement. No server or storage capacity is added.
The Flux controllers' placements live in the production FluxInstance's existing
controller patches, because the FluxInstance reconciles in its own layer. The
security-progress and policy-reporting UI patches live in the production
infrastructure layer. The Hubble UI patch lives in its controller layer, and
the contract probe uses the separate component in the production apps layer.
The collector's single-writer Recreate rollout briefly pauses security-progress metrics.

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

The dashboards and contract probe need three Ready, schedulable migration hosts
for their replicas and rolling surge. Before taking any of those hosts out of
service, replace the temporary placement through review. Verify the replacement
hosts' request and resident-memory budgets and wait for both replicas of each
workload to become Ready before starting host maintenance.

After the migration's runtime and backup checks pass, remove the temporary
controller and dashboard placements, the FluxInstance patches and the contract
probe component through review under #6432. Verify ordinary placement has
sufficient headroom before rebuilding or retiring any temporary CSI or Velero
target.
