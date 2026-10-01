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

The release pins both the signed chart and controller image by immutable digest.
The registry workspace is standard behavior and the sample is enabled. Portable UI
status, resize and appearance grants are active for the publisher-approved registry
origin, so System, Light and Dark appearance can reach the sandboxed sample without
resetting its query. Provisioning, engine providers, connectors, composition, DCAT
publication and contract probing remain disabled. The trial
has no database or persistent data volume. New capabilities and releases go through
reviewed GitOps changes before activation.

Flux owns the release, API definition, product registration, workloads, and routes.
Helm installs the DataProduct API before registering the sample, so a clean rebuild
does not depend on a pre-existing CRD. The chart's broad network policies and direct
registry route are removed by its post-renderer. Platform policies admit the registry
only from the authenticated proxy and the sample only from the Gateway.

Recovery uses a reviewed change to the pinned release or its values while retaining
the Namespace and HelmRelease. Both are protected from pruning; removing the app
reference alone does not uninstall the release. Keep the app reference and its
credential fan-out entry consistent.

After each deployment, verify the OCI source signature and Ready condition, release
readiness, exact running image, current route generations, and healthy workloads.
Anonymous registry access must require SSO; the sample query and contract must work
directly. Finish with an authenticated registry-to-sandbox query in the browser.
