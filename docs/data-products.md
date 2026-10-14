# Data product trial

The **Data Products** tile on Homepage opens the registry behind the platform's
maintainer-team SSO. Select **KSail releases** for live version and publication
dates, or **Harbour observations** for the synthetic reference. Each product opens
its independently served UI in the registry's opaque sandbox. The registry discovers
the product descriptor; it does not compile or copy the product's UI.

The registry is served at `https://data-products.${domain}`. The fixed Harbour
sample is public at `https://harbour-data.${domain}/ui`; its query API is
`/api/observations`, and its OpenAPI contract is `/openapi.json` on that same host.
The sample route removes cookies and authorization headers before forwarding.
It contains synthetic observations and requires no data-source credentials.

KSail's release view is public at
`https://harbour-data.${domain}/ksail-releases/ui`. Its browser reads the fixed
public `https://api.github.com/repos/devantler-tech/ksail/releases` endpoint with
no cookies or credentials. Version and channel filters use the loaded page of
at most 30 releases; Refresh reads GitHub again. An unavailable, partial or invalid
response clears the rows and shows a retryable error. The controller and product
workload do not fetch or store release records. The product's OpenAPI contract is
`/ksail-releases/openapi.json` on the same public host and names GitHub as the server.
Catalog readiness confirms registration and serving state; it does not claim that
GitHub is reachable from every consumer's browser.

Open `https://product-ui.${domain}` to try a product UI outside the registry. Paste
the product's UI descriptor and select its declared permissions before connecting.
The host only loads the declared HTTPS UI in an opaque sandbox. It does not fetch
the product's data or reuse registry credentials. The Harbour sample approves both
the registry and this host. Status, resize and appearance permissions are explicit;
changing the connected product starts a new session.

Select **Publisher review** on that host to inspect a saved `product-check` v2 JSON
report. Open a local file or paste the report; the browser reads it locally without
uploading it, fetching sources or applying resources. Review findings, public
descriptor previews, declared feature requirements and the dependency plan. The
selected System, Light or Dark theme follows navigation between the two workspaces.
Leaving publisher review clears the report; browser Back returns to an empty review.

Publisher review is enabled only on the independent UI host through
`PUBLISHER_PREFLIGHT_ENABLED`. Withdraw it with an absent or literal `false` value
in a reviewed GitOps change. This removes its navigation and publisher routes while
retaining the product UI workspace. Availability on the platform does not establish
independent-author adoption or authorize retiring the publisher release gate.

The release pins both the signed chart and controller image by immutable digest.
The registry workspace is standard behavior and both product views are enabled. Portable UI
status, resize and appearance grants are active for the publisher-approved registry
origin, so System, Light and Dark appearance can reach the sandboxed sample without
resetting its query. Provisioning, engine providers, source exports, composition
and DCAT publication remain disabled. The trial
has no database or persistent data volume. New capabilities and releases go through
reviewed GitOps changes before activation.

On v3.0.0, `demoProduct.ksailReleases.enabled` controls the registry descriptor.
Disabling it removes the listing while the direct release UI, contract and
configuration routes remain available on the existing reference workload.
The retired `KSAIL_RELEASES_ENABLED` environment variable has no runtime effect and
must be absent from the Deployment, current ReplicaSet and serving Pods. Withdrawing
the interface itself requires restoring a previously verified legacy image/chart
pair and disabling its gate through a reviewed GitOps change. See the
[retirement decision](adr/ksail-release-gate-retirement.md) for the artifact profiles
and recovery procedure. Source availability and independent publisher acceptance
remain separate adoption decisions.

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
pass. The receipt binds the portable host's contract and appearance opt-ins and its
declared publisher state to the UI Deployment, current ReplicaSet and serving Pods. Publisher
withdrawal permits an absent or literal false flag; indirect, duplicate and malformed
flag declarations are refused. The public checks require navigation between both
enabled workspaces, the offline publisher page, its scripts and stylesheet, and
their no-store and no-network policies. Withdrawal requires the publisher routes
to return 404. Local report interaction and navigation cleanup are additionally
verified in a browser.
The receipt binds the checked-out image tag/digest and chart digest to the supplied
identities and installed release. The retired profile accepts only the verified
v3.0.0 pair, requires the obsolete environment variable and `envFrom` to be absent
from the product Deployment, current ReplicaSet and serving Pods, and always checks
the three direct release routes. Older releases retain the literal gate checks.
Scaled-down historical ReplicaSets may keep their old image and flag; they cannot
serve Pods. The listing value still binds the checkout to the live Helm value,
current registration, exact source/contract URLs and approved host origins.
When unlisted, the descriptor must be absent; an unreadable resource is a refusal,
not proof of withdrawal. `ksailReleasesEnabled` in the receipt reports listing state;
`ksailReleaseGateRetired` reports the accepted artifact profile.
It performs no GitHub source read; that check belongs to the
consumer browser. Finish with an authenticated registry-to-sandbox query and a separately
connected portable-host query in the browser, including Light and Dark appearance.
