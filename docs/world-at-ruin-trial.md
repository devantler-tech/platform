# World at Ruin private zone trial

The tenant registers an owner-only scripted zone trial in the
`world-at-ruin` namespace. It runs the existing deterministic demo world and its
authenticated TLS WebSocket stream. It has no public HTTP route, external Service,
database, persistent volume, player account or progression integration.

World at Ruin owns the zone image and tenant manifests. Flux accepts stable
release artifacts signed by that repository's `server-cd.yaml`; both Kyverno
admission and Talos pulls verify the same exact image repository and signing
identity. Other application images retain their existing publisher boundary.

The platform owns the namespace, its default-deny network policy, resource budget,
shared registry pull secret, tenant identity and OpenBao policy. Tenant artifacts
do not create standard NetworkPolicies; the reconciliation identity can only read
those platform-owned policies. The tenant seeds and reads its admission key
through its namespaced `world-at-ruin` SecretStore, confined to
`apps/world-at-ruin/*`. The platform issues the separate TLS Secret through its
existing DNS-01 ClusterIssuer, so issuance needs no publicly reachable listener.
The process restarts each hour to reload its mounted TLS certificate.

## Owner access

The namespace's `world-at-ruin-zone-trial-operator` Role is a deliberate trial exception for
the configured administrator's existing OIDC identity. It grants only creation
of `pods/portforward` and `pods/exec` in this namespace. Global reader roles and
all other namespaces retain their existing access. There is no Secret API grant,
workload creation, RBAC administration or impersonation grant.

The tunnel binds to localhost. The launcher uses exec to run `/zone -mint-token`
inside the zone container, captures its short-lived bearer token privately and
never prints the token or underlying admission secret. Kubernetes RBAC cannot
restrict an exec grant to one command: this operator can execute any available
command in these trial containers. Retire the operator Role and binding before
adding production credentials, real player data or authoritative persistence.

The client verifies the public certificate chain and the issued server hostname
while connecting through localhost. It must not disable TLS verification, add a
public DNS record pointing at localhost or change workstation host mappings.

## Delivery and readback

Publish and verify both signed game packages before admitting the platform changes
to the production merge queue. First deliver the scoped image signer with the
tenant and namespace fanout disabled, then prove the exact rule is running on every
Talos node. The ordinary deploy waits for Flux before syncing Talos machine config;
enabling the tenant in that first delivery could block its image pull and prevent
the machine config sync. A second reviewed change enables the tenant and its
matching pull-credential fanout together. That deploy establishes the namespace,
certificate, secrets and Flux tenant; no manual apply is necessary.
Verify the tenant's exact artifact and image digest, Ready certificate,
ExternalSecrets and Deployment, then prove invalid admission is rejected and the
released client consumes the zone stream over the verified TLS tunnel. Observe
the trial through the existing platform workload and log views.

Removing the prod app-layer reference stops delivery. Namespace pruning remains
disabled by the platform's persistence-safety component. The host's namespace-wide
default-deny policy also disables pruning, so tenant removal or rollback cannot
remove isolation while its pods are still terminating. Retiring the trial
namespace, retained host policy or generated credentials requires explicit cleanup
after every workload and pod has been removed. The trial establishes deployment and replication evidence;
it does not activate the production Agones/Nakama handoff.
