# Parallel nested-template validation

## Decision

Run the complete nested ResourceGraphDefinition template regression in a
read-only PR job alongside manifest validation, selected by the existing `k8s`
filter. Keep its scanner version, action pin, finding baseline and behavioral
controls unchanged. The required aggregate waits for this job and separately
requires success whenever selected; missing evidence and unexpected skips fail.

The PR-only job is not a production prerequisite. Merge-group validation and
the current-main scan before recovery deployment retain their existing order.

## Rationale

Platform CI still exceeded fifteen minutes after earlier independent families
were separated. The nested scan occupied 168–180 seconds inside the serial
manifest job in three comparable successful runs. It needs only repository
files and pinned offline tools, not a cluster, KSail or other validation results.
Moving this work removes an unnecessary dependency without reducing coverage.

Parallelism can shorten feedback while increasing setup and occupied runner
time. It does not by itself prove that all checks finish within fifteen minutes.

## Validation and recovery

Workflow-graph mutation tests reject serialization, widened permissions and
missing required dependencies. Tests execute the actual result guard against
selected, unselected, failed, cancelled and unknown observations. The existing
scanner regression and deployment-route wiring controls remain mandatory.

Compare complete hosted run metadata before and after, reporting feedback and
occupied runner time separately. Revert this scheduling change if the extra
runner/setup cost outweighs its measured benefit; never remove the scan.
