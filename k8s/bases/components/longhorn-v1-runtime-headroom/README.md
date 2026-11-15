# V1 runtime rollout headroom

This component is inactive. It supplies temporary placement for the V1 runtime
rollout in #6073: Velero's server and Reloader use the existing migration worker
pools, while Hetzner CSI controllers use positively labelled baseline workers.
Their replicas, resource requests, disruption budgets, security settings and
replica-spreading rules remain configured by their owning releases. Node agents
keep their existing placement. No server or storage capacity is added.

Activate it only in the Hetzner controllers overlay, through a separately reviewed
change. Before rollout, verify the declared migration pools are Ready and have
room for both Reloader replicas on separate nodes and Velero's single server.
Verify the CSI controllers can spread across the baseline workers. Wait for
active backup and restore operations to finish before moving Velero; its
single-server Recreate rollout briefly pauses backup orchestration.

After rollout, require every affected Deployment to reach its current desired
replicas, each disruption budget to select its workload, and normal volume
attachment to remain healthy. Re-run the complete native runtime census.
Placement itself proves neither overlap capacity nor adoption of the approved
instance-manager image. A remaining HOLD still blocks adoption.

Remove this temporary placement through a reviewed change after runtime adoption
reserves its actual requests and before retiring either migration pool. Keep
that cleanup within #6073; removing a pool while these controllers depend on it
would leave them unschedulable. Returning to the preceding Git configuration
restores their ordinary placement.
