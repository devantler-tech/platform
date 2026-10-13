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

## Validation

Use the pinned admission engine with positive and adversarial identity fixtures,
the published native Flux/Helm adapter, and server-side dry-run controls before
protected production execution. Policy presence alone is not enforcement proof.
The workflow must fail closed on incomplete evidence, ineffective admission,
changed retained content, or a foreign operation. Offline controls prove their
bounded behavior, not production controller or garbage-collector completion.
