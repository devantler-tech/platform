# ADR: Independent data product readiness observation

- **Status:** Accepted; observation is prepared and disabled.
- **Tracks:** [Platform #4584](https://github.com/devantler-tech/platform/issues/4584)
  and [Data Product Controller #4](https://github.com/devantler-tech/data-product-controller/issues/4).

The registry's Harbour sample publishes its contract through a public route. A
healthy controller does not establish that this independently served workload or
its contract is available.

Platform owns a separate contract-probe Deployment, with zero replicas and
literal observation disabled. The controller's connector and contract flags stay
disabled, and the sample declares neither dependency. Activation requires a
separate GitOps change to all four settings: controller flags, product references,
probe replicas, and probe execution.

The dormant workload uses the replica-floor policy's documented scale-to-zero
label. Activation removes that label and runs two probes, so product availability
does not depend on a single observation Pod.

The probe targets only the sample's public HTTPS contract. Its service account
does not mount an API token; it has no credential volumes, management Service,
or public route. Cilium permits only this hostname on TCP 443 and its DNS lookup
through cluster DNS. Its readiness endpoint checks contract reachability; its
liveness endpoint checks the process independently.

A namespace Role grants the controller only GET on the named Harbour and probe
Deployments. The controller observes workload status without adopting workloads
or performing network probes itself. Keeping contract probing separate avoids
making Harbour's readiness depend on its own Gateway endpoint.

The deployment verifier reads the exact observer grant, probe configuration,
network policy, and feature state in both snapshots surrounding public checks.
An incomplete read or changing rollout cannot produce a complete receipt.
Dormancy requires current zero-replica status and no owned Pods; a disabled probe
is never made Ready by substituting its liveness endpoint.

Enabling observation proves only the healthy adoption path. Fault detection,
recovery, access revocation, and rollback remain separate acceptance work before
connector or contract flag retirement.
