# Apps

Applications that run on the platform. They differ from
[infrastructure](../infrastructure) in that they serve users directly and
provide no capability other workloads depend on.

[`kustomization.yaml`](kustomization.yaml) is the list of what is included.
A folder that it does not list is not included by this base.

| Folder                                                | Status                                                                                      |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| [`actual-budget/`](actual-budget)                     | Active. Can also be switched on locally.                                                    |
| [`ascoachingogvaner/`](ascoachingogvaner)             | Active in production only. Tenant: its manifests ship from its own repository.              |
| [`data-product-controller/`](data-product-controller) | Active in production only.                                                                  |
| [`fleetdm/`](fleetdm)                                 | Retained, not deployed since 3 June 2026. See its [README](fleetdm/README.md).              |
| [`github-config/`](github-config)                     | Active in production only. See [GitHub management](../../../docs/github-management.md).     |
| [`homepage/`](homepage)                               | Active. Can also be switched on locally.                                                    |
| [`umami/`](umami)                                     | Active in production only; its database needs production storage.                           |
| [`wedding-app/`](wedding-app)                         | Active in production only. Tenant.                                                          |
| [`whoami/`](whoami)                                   | Active. Can also be switched on locally.                                                    |

The production overlay
([`providers/hetzner/apps`](../../providers/hetzner/apps)) includes this whole
base and adds production-only apps. The local overlay
([`providers/docker/apps`](../../providers/docker/apps)) deploys no apps by
default; its `kustomization.yaml` carries the lines to copy for the three apps
that can run locally.

To host a new application, follow [tenant onboarding](../../../docs/TENANTS.md).
