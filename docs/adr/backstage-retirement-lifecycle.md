# Backstage retirement lifecycle

## Decision

Keep the unsuspended, published Flux HelmRelease finalizer and its native Helm
uninstall. Before submitting owner deletion, install a durable, namespace-bound
retirement journal and require an enforced admission fence. The fence permits
native deletion only for the captured stateless target identities and prevents
same-name recreation. Retained data cannot acquire retiring owners or be deleted.
The journal contains identities and operation provenance, never Secret values.

A failed or timed-out workflow does not cancel a Kubernetes finalizer. After an
owner DELETE may have reached the API server, HOLD means the native lifecycle may
continue. Keep the fence and consumed attempt, inspect the existing operation,
and do not start another retirement. The fence is not automatically cleared.

## Rationale and bounds

Flux 1.5.5 associates generated HelmCharts with labels rather than mandatory owner
references. Its chart cleanup and Helm 4.2.0's resource deletion use names without
the script's UID preconditions. A final snapshot alone therefore cannot protect a
replacement during later reconciliation. API admission must constrain that later
deletion, not merely detect damage afterward.

Read and decode every stored Helm revision under a bounded in-memory limit.
Require old revisions already non-deployed and preserve their exact bytes. Only
the latest release may change native uninstall information. Its Secret update
exception is confined to the captured UID and published controller's identity;
Kyverno does not decode gzip. Normalized decoded content is independently checked
during readback, not represented as a payload-validation admission guarantee.

Retained owner-reference checks cover all captured owners and descendants,
including volumes. Retained claims require prune protection before lifecycle
writes. Do not freeze all database Pod deletion: CloudNativePG must remain able
to replace a failed instance. No guarantee depends on Kyverno's filtered
ReplicaSet kinds. New retained data retirement is a separate reviewed operation.

Do not turn the durable retained-data fence into a controller freeze. The
protected capture binds generated-resource profiles to the actual producer
ServiceAccount UIDs and verifies the published CNPG and trust-manager versions.
Allow only CNPG's public CA/leaf certificate field to renew; private-key bytes,
Secret types, key sets, ownership and unrelated metadata remain unchanged.
Readback verifies key matching, issuer signatures and stable subject, issuer,
public key and extensions, rather than silently ignoring certificate data.

For captured CNPG claims, permit only the paired primary/replica role labels.
Readback derives both claim and Pod role labels from the healthy, settled
Cluster primary; Pod UID, spec and other metadata remain checked. Public root-CA
and trust-bundle profiles permit only their single certificate-bundle key and
the producer's specific bookkeeping annotation. Trust-bundle hashing assumes
the captured target has no additional formats or metadata template. Monitoring
refresh must retain its one-key shape and match the operator's observed source.
No profile permits credential-data changes, storage rebinding, owner changes,
deletion, recreation, or arbitrary annotations.

Before fencing, prove the default credential and certificate metadata already
matches the published constructor plus the frozen Cluster's inherited metadata.
The existing credential constructor then performs a metadata no-op and never
replaces its password. Reject unsettled entries and uncharacterized inheritance
overrides before writing a journal, leaving the operator free to reconcile them.

Initial separated samples still compare full content. Profile normalization
starts only after the exact owned journal has been installed and read back.
An unsupported producer version, changed account identity, source/template
shape, unhealthy failover or replacement Pod is HOLD, not permission to broaden
the profile or claim that a submitted native finalizer has been cancelled.

## Validation

Use the pinned admission engine with positive and adversarial identity fixtures,
the published native Flux/Helm adapter, and server-side dry-run controls before
protected production execution. Policy presence alone is not enforcement proof.
The workflow must fail closed on incomplete evidence, ineffective admission,
changed retained content, or a foreign operation. Offline controls prove their
bounded behavior, not production controller or garbage-collector completion.

An unavailable retired chart is not an active Helm action. The pinned controller
marks prerequisite waiting as reconciling before checking the source, while its
deletion branch runs first. Accept only the exact current-generation source-wait
profile, joined to the unavailable chart and unchanged deployed storage/history.
Reject stale, duplicate, missing or active-action conditions. Recheck this profile
after the uninstall-options patch; a changed source or release still stops owner
deletion. No chart restoration or synthetic credential is needed for retirement.

## Native-driver security exception

The test-only native driver deliberately locks the published helm-controller
1.5.5, Helm 4.2.0 and SSA dependency graph. Updating its indirect dependencies
would test a different implementation, not the one production runs. This is not
a claim that vulnerable symbols are unreachable: `govulncheck` identifies
`GO-2026-5026` through `action.Uninstall` to `idna.ToASCII` in `x/net` 0.53.0.
The advisory is fixed in `x/net` 0.55.0, outside this published graph.

Accept this narrowly scoped exception only for the offline proof: the driver
requires a literal loopback IP and constructs anonymous API clients. The controlled
proof supplies synthetic resources and a temporary anonymous configuration, with
no production credential or remote API. An ASCII/Punycode hostname cannot pass
the driver's IP guard. The complete scan also
reports other reachable HTTP/HTTP2/standard-library advisories; this is not a
vulnerability-free dependency claim or an exception for production binaries.
Do not suppress the scan or upgrade a lock independently. Re-audit and remove
the exception when the producer advances to a fixed published graph, or stop
using the driver if its loopback-only, anonymous-fixture boundary changes.

Native waiter result markers distinguish success from a deadline-only failure.
A storage/API error, including one joined with a deadline, is not a successful
negative timeout control.
