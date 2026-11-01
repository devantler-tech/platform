# KHoist deployment trial

The **KHoist** tile on Homepage opens the registry behind the platform's
maintainer-team SSO. The registry lists products and their named outputs, contracts
and readiness. It runs no product UI and queries no output by itself: an application
integrates with a product through the product's standard API. See the
[adoption decision](adr/khoist-api-only-adoption.md) for what changed, why, and rollback.

[KHoist](https://github.com/devantler-tech/khoist) is the product's canonical name.
Existing namespaces, release names, workload selectors and the public registry address
keep their historical `data-product-controller` names.

The registry is served at `https://data-products.${domain}`. The fixed Harbour
sample's query API is public at `https://harbour-data.${domain}/api/observations`,
and its OpenAPI contract is `/openapi.json` on that same host. The sample route removes
cookies and authorization headers before forwarding. It contains synthetic observations
and requires no data-source credentials.

KSail's release page is public at
`https://harbour-data.${domain}/ksail-releases/ui`. Its browser reads the fixed
public `https://api.github.com/repos/devantler-tech/ksail/releases` endpoint with no
cookies or credentials. The controller and product workload do not fetch or store release
records. The product's OpenAPI contract is `/ksail-releases/openapi.json` on the same
public host and names GitHub as the server. Catalog readiness confirms registration and
serving state; it does not claim that GitHub is reachable from every consumer's browser.

Open `https://publisher-review.${domain}` to inspect a saved `product-check` v2 JSON
report. Open a local file or paste the report; the browser reads it locally without
uploading it, fetching sources or applying resources. Review findings, public descriptor
previews, declared feature requirements and the dependency plan.

Publisher review is enabled through `PUBLISHER_PREFLIGHT_ENABLED` on its own application.
Withdraw it with an absent or literal `false` value in a reviewed GitOps change, which
removes its pages. Availability on the platform does not establish independent-author
adoption or authorize retiring the publisher release gate.

The release pins both the signed chart and controller image by immutable digest.
Provisioning, engine providers, source exports, composition and DCAT publication remain
disabled. The trial has no database or persistent data volume. New capabilities and
releases go through reviewed GitOps changes before activation.

`demoProduct.ksailReleases.enabled` lists the release page and its descriptor in the
registry on the existing two-replica reference workload. It adds no workload,
service-account grant or cluster egress. Disable that value in a reviewed GitOps change to
delist the product; the page itself stays served by this release.

Harbour readiness observes its serving Deployment and a separate two-replica
contract probe. The probe uses a token-free service account and an exact-host
HTTPS and DNS policy; the controller can GET only the named Harbour and probe
Deployments. All desired replicas must be current and ready. The deployment
receipt requires current dependency conditions, the declared observations and contract URLs,
and healthy probe Pods, and refuses a state that differs from the checked-out
Helm declaration. See the [readiness decision](adr/data-product-readiness-observation.md)
for rollback and the remaining fault/recovery acceptance before flag retirement.

Flux owns the release, API definition, product registration, workloads, and routes.
Helm installs the DataProduct API before registering the sample, so a clean rebuild
does not depend on a pre-existing CRD. The chart's broad network policies and direct
registry route are removed by its post-renderer. Platform policies admit the registry
only from the authenticated proxy, and the sample and publisher application only from the
Gateway. The publisher application has no outbound network access, service-account token,
or data-source credentials. Two replicas keep it available during ordinary updates.

Recovery uses a reviewed change to the pinned release or its values while retaining
the Namespace and HelmRelease. Both are protected from pruning; removing the app
reference alone does not uninstall the release. Keep the app reference and its
credential fan-out entry consistent.

Every deployment checks the exact published Apps revision, signed chart, installed
release and running image, current route generations, and healthy current Pods.
Anonymous registry access must require SSO; the sample query and contract must work
directly. The publisher application's health endpoint, pages and no-network policy must
also pass.

The receipt binds the deployed digests to the image and chart declared in this checkout,
and the publisher state to its Deployment, current ReplicaSet and serving Pods. Withdrawal
permits an absent or literal false flag and requires its pages to return 404; indirect,
duplicate and malformed flag declarations are refused. No product may declare `spec.ui`,
and the sample's Deployment, current ReplicaSet and Pods may not carry the retired KSail
flag. When KSail releases are delisted, the receipt requires the descriptor to be absent;
an unreadable resource is a refusal, not proof of withdrawal. It performs no GitHub source
read; that check belongs to the consumer browser.

Finish in a browser: read a product through the authenticated registry, query an output
from a separate application, and load a report in publisher review, including Light and
Dark.
