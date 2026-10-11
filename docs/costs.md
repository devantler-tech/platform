# Platform costs

Low infrastructure cost is a design goal, alongside redundancy, backups, and
repeatable deployments. This is self-managed infrastructure used for real
business services, not a managed Kubernetes service or an uptime guarantee.

## Baseline budget

Public prices checked **11 October 2026**, in EUR excluding VAT, for Germany.
This is a new-instance estimate, not the maintained platform's invoice.
Compute sizing follows [`ksail.prod.yaml`](../ksail.prod.yaml); networking
assumes one Primary IPv4 per node, one Floating IPv4 for the API endpoint, and
the configured LB11 load balancer.

| Resource                 | Count | Unit/month |   Subtotal |
| ------------------------ | ----: | ---------: | ---------: |
| CX33 control plane       |     3 |      €8.49 |     €25.47 |
| CX43 worker              |     3 |     €15.99 |     €47.97 |
| LB11                     |     1 |      €7.49 |      €7.49 |
| Primary IPv4             |     6 |      €0.50 |      €3.00 |
| Floating IPv4            |     1 |      €3.00 |      €3.00 |
| **Compute + networking** |       |            | **€86.93** |

Rates come from Hetzner's [server price list][servers],
[load-balancer pricing][load-balancers], and [IPv4 pricing][ipv4]. Servers and
load balancers are billed hourly up to their monthly caps. Budget from current
public rates rather than assuming a legacy price will survive a replacement.

## Add storage and usage

The subtotal above deliberately excludes these variable costs:

- **Persistent storage:** Longhorn keeps its replicas on each worker's
  included local disk, so replicated volumes add no separate charge. Their
  capacity is bounded by that disk, and replication, snapshots, and free-space
  headroom reduce it further; three workers do not give three disks of
  application data. Hetzner [block storage][volumes] is charged only for
  volumes created through the Hetzner CSI driver, at €0.044 per provisioned
  GB-month. As an illustration, three 50 GB volumes would add **€6.60/month**,
  taking compute, networking, and that storage to **€93.53/month** before VAT
  and the other items below. This is an example, not a claim about live volume
  sizes.
- **Backups:** Cloudflare [R2 Standard storage][r2] costs $0.015 per GB-month
  beyond its free allowance; requests are charged separately. Retention,
  database write volume, and restore activity affect the bill. Check existing
  R2 usage before budgeting from a free allowance.
- **Burst capacity:** autoscaled CX43 and CX53 workers are extra, billed for
  their lifetime. Their monthly server caps are €15.99 and €29.49 respectively,
  plus allocated IPv4. Pools can scale to zero; a retained resource still costs
  money even if its workloads are idle.
- **Other charges:** domains, provider snapshots, extra traffic, any paid
  service plans, temporary rollout resources, and taxes. R2 prices are in USD;
  currency conversion is not included in the EUR estimate.

Choose storage and backup retention for your recovery needs, not merely to
reach a headline price. See [storage setup](rwx-storage.md) and
[backups and recovery](dr/README.md).

## Why the price is low—and what you take on

The baseline uses shared-vCPU servers and open-source components, without a
separate managed Kubernetes control-plane fee. Shared CPU is economical for
variable, modest workloads; sustained CPU-heavy workloads may need larger or
dedicated resources. See Hetzner's [shared versus dedicated guidance][cloud].

For context, [Amazon EKS][eks] charges $0.10 per cluster-hour for standard
version support: $73 for a 730-hour month, before workers, storage, or networking.
That is one managed-service fee, not a like-for-like total-cost comparison.
Managed services take on responsibilities this platform leaves with its operator.

Redundant nodes and replicated storage are production foundations, not a
substitute for workload sizing, upgrades, security maintenance, monitoring,
tested restores, and incident response. Include that work in your budget;
the [architecture guide](architecture.md) describes the design and environment
differences.

[servers]: https://docs.hetzner.com/general/infrastructure-and-availability/price-adjustment/#cloud-servers
[load-balancers]: https://www.hetzner.com/cloud/load-balancer/
[ipv4]: https://docs.hetzner.com/general/infrastructure-and-availability/ipv4-pricing/
[volumes]: https://www.hetzner.com/cloud/block-storage/
[r2]: https://developers.cloudflare.com/r2/pricing/
[cloud]: https://www.hetzner.com/cloud/
[eks]: https://aws.amazon.com/eks/pricing/
