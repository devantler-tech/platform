# ADR: Independent data product readiness observation

- **Status:** Accepted; Harbour observation is enabled.
- **Tracks:** [Platform #4590](https://github.com/devantler-tech/platform/issues/4590),
  [Platform #4584](https://github.com/devantler-tech/platform/issues/4584)
  and [Data Product Controller #4](https://github.com/devantler-tech/data-product-controller/issues/4).

The registry's Harbour sample publishes its contract through a public route. A
healthy controller does not establish that this independently served workload or
its contract is available.

Platform owns a separate contract-probe Deployment with two replicas and literal
observation enabled. The controller's connector and contract flags are enabled for
the Harbour adoption, and the sample names its serving Deployment and independent
probe. The chart-owned probe remains disabled so a second probe does not share
the serving lifecycle. Two replicas meet Platform's capacity floor; all desired
probes must be ready for product readiness.

The probe targets only the sample's public HTTPS contract. Its service account
does not mount an API token; it has no credential volumes, management Service,
or public route. Cilium permits only this hostname on TCP 443 and its DNS lookup
through cluster DNS. Its readiness endpoint checks contract reachability; its
liveness endpoint checks the process independently.

A namespace Role grants the controller only GET on the named Harbour and probe
Deployments. The controller observes workload status without adopting workloads
or performing network probes itself. Keeping contract probing separate avoids
making Harbour's readiness depend on its own Gateway endpoint.

The deployment verifier derives the expected observation state from the
checked-out Helm declaration. It reads the exact observer grant, probe
configuration, network policy, feature state and product references in both
snapshots surrounding public checks. Active acceptance requires current unique
ConnectorReady, ContractsReady and aggregate Ready conditions with their expected
reasons, the current output URL, and two current ready probe Pods. It checks the
effective Pod configuration as well as the Deployment template.
An incomplete read or changing rollout cannot produce a complete receipt.
For an explicit rollback, the dormant profile requires both controller flags off,
no product dependency references, literal probe execution disabled, current zero
capacity and no owned probe Pods. The deliberate scale-to-zero policy label
applies only while dormant. All four settings change together through GitOps; a
disabled probe is never made Ready by substituting its liveness endpoint.

Enabling observation proves only the healthy adoption path. Fault detection,
recovery, access revocation, and rollback remain separate acceptance work before
connector or contract flag retirement.
