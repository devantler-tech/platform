# Data product trial

The **Data Products** tile on Homepage opens the registry behind the platform's
maintainer-team SSO. Select **Harbour observations** to open its independently
served UI in the registry's opaque sandbox, then run a query. The registry discovers
the product descriptor; it does not compile or copy the product's UI.

The registry is served at `https://data-products.${domain}`. The fixed Harbour
sample is public at `https://harbour-data.${domain}/ui`; its query API is
`/api/observations`, and its OpenAPI contract is `/openapi.json` on that same host.
The sample route removes cookies and authorization headers before forwarding.
It contains synthetic observations and requires no data-source credentials.

Open `https://product-ui.${domain}` to try a product UI outside the registry. Paste
the product's UI descriptor and select its declared permissions before connecting.
The host only loads the declared HTTPS UI in an opaque sandbox. It does not fetch
the product's data or reuse registry credentials. The Harbour sample approves both
the registry and this host. Status, resize and appearance permissions are explicit;
changing the connected product starts a new session.

The release pins both the signed chart and controller image by immutable digest.
The registry workspace is standard behavior and the sample is enabled. Portable UI
status, resize and appearance grants are active for the publisher-approved registry
origin, so System, Light and Dark appearance can reach the sandboxed sample without
resetting its query. Provisioning, engine providers, connectors, composition, DCAT
publication and contract probing remain disabled. The trial
has no database or persistent data volume. New capabilities and releases go through
reviewed GitOps changes before activation.

An independent contract-probe Deployment is prepared with zero replicas and
literal observation disabled. Its token-free service account, exact-host HTTPS
and DNS policy, and controller GET permission on just the Harbour and probe
Deployments are installed without changing the sample's readiness. The deployment
receipt requires this dormant state and rejects broader grants, target changes,
or unexpected activation. See the [readiness decision](adr/data-product-readiness-observation.md)
for the activation boundary.

Flux owns the release, API definition, product registration, workloads, and routes.
Helm installs the DataProduct API before registering the sample, so a clean rebuild
does not depend on a pre-existing CRD. The chart's broad network policies and direct
registry route are removed by its post-renderer. Platform policies admit the registry
only from the authenticated proxy, and the sample and portable host only from the
Gateway. The portable host has no outbound network access, service-account token,
or data-source credentials. Two replicas keep it available during ordinary updates.

Recovery uses a reviewed change to the pinned release or its values while retaining
the Namespace and HelmRelease. Both are protected from pruning; removing the app
reference alone does not uninstall the release. Keep the app reference and its
credential fan-out entry consistent.

Every deployment checks the exact published Apps revision, signed chart, installed
release and running image, current route generations, and healthy current Pods.
Anonymous registry access must require SSO; the sample query and contract must work
directly. The portable host's health endpoint, assets and sandbox policy must also
pass. Finish with an authenticated registry-to-sandbox query and a separately
connected portable-host query in the browser, including Light and Dark appearance.
