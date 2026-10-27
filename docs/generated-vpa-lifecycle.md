# Generated VPA lifecycle

The three auto-VPA GeneratingPolicies create an autoscaler in the workload's
namespace with an owner reference to that workload's actual UID. A replacement
with the same name is a different owner. Kubernetes garbage collection can remove
the previous owner's autoscaler; Kyverno generates the replacement for the current
owner. No controller or blockOwnerDeletion permission is added.

The ownership change is backported from reviewed kyverno-policies revision
8a36995dcf62df42448f494e024681533ebbe48a. The Platform keeps its existing
name formula, namespace exclusions, update modes and resource bounds. Both consumer
discovery guards allow only the complete reviewed generators, without CEL
variables, arbitrary resource reads or additional generated objects.

## Existing ownerless autoscalers

The source change does not make a deleted workload emit another event. The
**Clean Up Orphan Generated VPAs** workflow handles legacy ownerless records after
the prevention is deployed. Dispatch it from main with mode assess first and
confirmation cleanup-orphan-generated-vpas. Review its aggregate result, then
dispatch mode apply with the same confirmation. A rerun requires a fresh dispatch.

Apply compares all three deployed generators with the reviewed dispatch revision
before considering deletion. The complete census is limited to 250 VPAs. Only
autoscalers with consistent Kyverno policy and trigger provenance are eligible;
objects with owner references, finalizers, deletion in progress or an existing
target are preserved. An existing target is preserved even when its UID differs
from the recorded trigger.

All initial reads must succeed before any deletion. A successful named
ignore-not-found read proves target absence; an access error, malformed response
or incomplete census is UNKNOWN. Each candidate's UID and resource version and
its target's absence are checked again immediately before a DELETE whose
preconditions contain that exact UID and resource version. The workflow verifies
absence after deletion. Any failure stops the operation; no workload, namespace,
storage or finalizer is deleted or modified. Public output contains aggregate
counts, while API response text stays private.

The absence checks and the preconditioned VPA deletion are separate API requests.
A workload can return between them; the deployed generator supplies the new
owner-bound autoscaler. The cleanup never overrides a failed precondition, and
should be assessed again after any partial run.

## Validation and deployment evidence

Run the policy fixtures with the repository-pinned Kyverno CLI, followed by
scripts/tests/test-auto-vpa-generating-policy-ownership.sh and
scripts/tests/test-auto-vpa-generating-policy-exclusion.sh. The ownership test
covers all three workload kinds, same-name replacement UIDs, rejection of the
previous owner and missing, empty and numeric UIDs. The cleanup regression uses a
fake API to exercise live targets, replacements, changed UIDs and versions,
returning targets, failed or partial reads, an undeployed prevention and survivors.
Both full consumer discovery guard suites must also pass.

After deployment, inspect generated autoscalers against the current workloads'
UIDs and confirm that a normal workload replacement receives an autoscaler bound
to its new UID. A synthetic fixture proves generation behavior, not production
garbage collection. Run the bounded cleanup only after its deployment gate passes,
then verify the obsolete target errors stop in the recommender's current logs
and clear in Coroot after the relevant observation window. A merged source
change or a green workflow alone is not proof that those symptoms recovered.
