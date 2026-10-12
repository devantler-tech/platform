# Apps

Applications that run on the platform. They differ from
[infrastructure](../infrastructure) in that they serve users directly and
provide no capability other workloads depend on.

[`kustomization.yaml`](kustomization.yaml) is the list of what is included.
A folder that it does not list is not deployed.

| Folder                                                | Status                                                                                      |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| [`actual-budget/`](actual-budget)                     | Active.                                                                                     |
| [`ascoachingogvaner/`](ascoachingogvaner)             | Active. Tenant: the application ships from its own repository.                              |
| [`crossview/`](crossview)                             | Active in production only; it reads Crossplane resources.                                   |
| [`data-product-controller/`](data-product-controller) | Active.                                                                                     |
| [`fleetdm/`](fleetdm)                                 | Retained, not deployed since 3 June 2026. See its [README](fleetdm/README.md).              |
| [`github-config/`](github-config)                     | Active in production only. See [GitHub management](../../../docs/github-management.md).     |
| [`headlamp/`](headlamp)                               | Active.                                                                                     |
| [`homepage/`](homepage)                               | Active.                                                                                     |
| [`umami/`](umami)                                     | Active.                                                                                     |
| [`wedding-app/`](wedding-app)                         | Active. Tenant.                                                                             |
| [`whoami/`](whoami)                                   | Active.                                                                                     |

The production overlay
([`providers/hetzner/apps`](../../providers/hetzner/apps)) includes this whole
base and adds production-only apps. The local overlay
([`providers/docker/apps`](../../providers/docker/apps)) deploys no apps by
default; its `kustomization.yaml` carries the lines to copy for the apps that
can run locally.

To host a new application, follow [tenant onboarding](../../../docs/TENANTS.md).
