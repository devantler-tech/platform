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
Record possible submission at the prepared transport invocation, not before its
source check or request preparation. Those earlier failures cannot claim an
ongoing native lifecycle. Recheck current main immediately before journal and
uninstall-options writes; this bounds staleness but is not a cross-system lock.

## Rationale and bounds

Flux 1.5.5 associates generated HelmCharts with labels rather than mandatory owner
references. Its chart cleanup and Helm 4.2.0's resource deletion use names without
the script's UID preconditions. A final snapshot alone therefore cannot protect a
replacement during later reconciliation. API admission must constrain that later
deletion, not merely detect damage afterward.

Read and decode every stored Helm revision under a bounded in-memory limit.
Require old revisions already non-deployed and preserve their exact bytes. Only
the latest release may change native uninstall information. Its Secret update
exception is confined to the captured Secret UID and actual controller
ServiceAccount UID, so a same-name replacement writer is not trusted;
Kyverno does not decode gzip. Normalized decoded content is independently checked
during readback, not represented as a payload-validation admission guarantee.

Retained owner-reference checks cover all captured owners and descendants,
including volumes. Retained claims require prune protection before lifecycle
writes. Do not freeze all database Pod deletion: CloudNativePG must remain able
to replace a failed instance. No guarantee depends on Kyverno's filtered
ReplicaSet kinds. New retained data retirement is a separate reviewed operation.

Retaining the database also retains its dedicated monitoring dependency and the
scrape-only network path, not the retired portal's access. External Secrets
creates the monitoring credential with `CreatedOnce`; the existing CNPG managed
role and Coroot both consume that same Secret. This prevents periodic generation,
not rotation after deleting and recreating the ExternalSecret. Restore and verify
this dependency before installing a retirement journal; the fixed credential
profile does not permit a later password change. Capture the Password generator
alongside the ExternalSecret and require its own versioned admission denial.

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

Before fencing, prove the default basic-auth credential metadata already matches
its published constructor plus the frozen Cluster's inherited metadata. The
existing credential constructor then performs a metadata no-op and never replaces
its password. Certificate constructors instead set managed-by and ownership;
they do not copy credential inheritance or operator-version annotations. Reject
unsettled credentials and uncharacterized inheritance or monitoring-source
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
Probe every retained non-Pod resource through its fixed, versioned API path,
including the database Cluster and retained CRDs. Unsupported API targets hold;
core-kind denials do not prove custom-resource webhook coverage.
The workflow must fail closed on incomplete evidence, ineffective admission,
changed retained content, or a foreign operation. Offline controls prove their
bounded behavior, not production controller or garbage-collector completion.

An unavailable retired chart is not an active Helm action. The pinned controller
marks prerequisite waiting as reconciling before checking the source, while its
deletion branch runs first. Accept only the exact source-wait profile, joined to
the unavailable chart and unchanged deployed storage/history. Ready and Reconciling
must share the observed generation or the single captured options-patch generation.
Released may describe an older successful action only when its exact message names
the latest deployed history revision and chart, whose name/version must also
match the captured stored release. Reject zero/future generations,
unjoined success messages, duplicate, missing or active-action conditions. Recheck this profile
after the uninstall-options patch; a changed source or release still stops owner
deletion. No chart restoration or synthetic credential is needed for retirement.

The chart-consumer fence applies only to actual consumers, even if a present
journal is unbound. API-context failures still fail closed; cluster-wide PV
admission therefore depends on the healthy webhook and its namespace-list
permission. Journal shape changes require a matching reviewed policy change.

## Native-driver security exception

The test-only native driver deliberately locks the published helm-controller
1.5.5, Helm 4.2.0 and SSA dependency graph. Updating its indirect dependencies
would test a different implementation, not the one production runs. The native
executable authenticates the complete
module and checksum locks, then checks every linked dependency and the producer's
intentional replacements against them. Renovate excludes this manifest before
extraction, because forced security updates override package-level disables.
Dependabot's all-version ignore is confined to the exact fixture directory;
version-only exclusions do not stop security updates. Alerts and scans remain
visible. The graph moves with a reviewed producer change and renewed proof (#4840).
This is not a claim that vulnerable symbols are unreachable: `govulncheck` identifies
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
