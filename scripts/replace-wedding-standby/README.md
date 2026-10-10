# Failed Wedding standby replacement

This command is restricted to the failed `wedding-db-1` replica. Its default mode is
an OIDC-only, read-only plan. Mutations require the first, explicitly confirmed
main dispatch of `Replace Failed Wedding Standby`, behind the `prod` environment
and the shared production deployment lock. Publishing or merging this procedure
does not authorize running it; the maintainer must approve the particular repair.

## Completed-join recovery

The initial keep-PVC repair did not restore three healthy instances. The audited
operator reuses the missing ordinal and can adopt its detached claim and completed
join Job. Preserving the claim name does not reserve a different replacement name.
The completed-join HOLD needs the separately approved `Recover Retained Wedding
Standby` workflow, not another attempt of the initial repair.

This continuation requires current identities for the Cluster, completed join
Pod and Job, detached claim, and retained volume. It proves the same two healthy
database peers and a recent completed backup. It pauses only this Cluster through
the [audited reconciliation annotation](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/internal/controller/cluster_controller.go)
and requires a fresh, structured acknowledgment from the bound operator leader.
Any leader change stops further writes; the annotation alone is not acknowledgment.
Leader renewals and acknowledgment timestamps allow at most two seconds of clock
skew; lease expiry is not extended. A bounded, structured log snapshot is read
before pausing, and any reconciliation ID already seen there is rejected as a
replay. Unreadable or malformed baseline logs stop before the first write.

While that pause remains exclusively owned, it removes only the exact completed
join Job and its completed dependent Pod, waits until every namespace Pod has
stopped referencing the old claim, then deletes the detached PVC with UID and
resource-version preconditions. The original PV is never deleted, rebound or
made available. Its existing `Retain` policy, original claim reservation and CSI
backing volume identity must remain unchanged. Kubernetes documents this
[released but retained state](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#retain).
The backing Longhorn Volume must also remain present, detached, nonterminating
and bound to its original UID at every stage; a retained PV alone is not enough.

Reconciliation resumes only after the old claim and Job are absent and the old
PV is Released, retained and reserved to the deleted claim's UID. The replacement
may reuse the ordinal's name, but must use a different claim UID, PV and backing
volume. Success requires two separated complete samples with three Ready instances,
unchanged healthy peers, healthy backups and the original volume still retained.
No primary, healthy standby, backup, disruption budget or underlying disk is
deleted or relaxed. Retention preserves the volume's current contents; it does
not prove that an earlier bootstrap left the original failure evidence untouched.

The default completed-join plan uses only OIDC reads:

```sh
go run ./scripts/replace-wedding-standby --quarantine-completed-join \
  --cluster-uid "$CLUSTER_UID" --pod-uid "$POD_UID" --job-uid "$JOB_UID" \
  --claim-uid "$CLAIM_UID" --volume-uid "$VOLUME_UID"
```

Execution requires a first main dispatch with confirmation
`retain-volume-rebuild-completed-standby`, the `prod` environment and the shared
deployment lock. Its tests run before any production credentials are restored.
Every write requires fresh observations and proof that its reviewed source remains
current main. Cluster patches re-read the exact Cluster after the other evidence
is gathered and revalidate its recovery predicates before using that final UID
and resource version. Concurrent status updates during earlier inventory reads
do not weaken those conditional tests; a change after the final read still fails
the single attempt. The protected completed-join and diagnostic paths resolve
their existing selected certificate context once, then use a persistent, fixed-path
Cluster connection for the final GET and PATCH. This removes subprocess discovery
and kubectl patch's additional target GET without changing any precondition.
Default OIDC planning and other resource operations still use kubectl. Unsupported
credential, impersonation or explicit proxy settings fail before API access;
CA verification and any configured TLS server name remain required. Credential
output is bounded in memory and is never printed or persisted.

The Cluster connection uses HTTP/1 and a non-replayable PATCH body, refuses
redirects, and retains the explicit repair field manager and server-side dry-run
query. Throttling and lost responses grant no retry. A shorter request window
does not guarantee acceptance during concurrent status updates or identify the
cause of an earlier rejection. HTTP 422 responses are read only in bounded memory
and classified by their verified Kubernetes Status envelope: `SERVER_INVALID_NO_CAUSES`
or `SERVER_INVALID_WITH_CAUSES`. Neither category identifies a failed predicate.
Malformed, incomplete, oversized or mismatched envelopes retain `SERVER_INVALID`;
response messages, fields and values are never printed or persisted.
A failed mutation or unknown read stops immediately without retry
or cleanup writes. The pause or retained volume may remain at HOLD; inspect them
read-only and obtain a separately reviewed and approved continuation. Never rerun
a consumed dispatch or delete the retained PV as cleanup.

## Non-persisting pause diagnostic

A rejected first pause request can be examined only through the separately
confirmed `Diagnose Wedding Pause` workflow. Its first current-main dispatch
requires `dry-run-retained-wedding-pause`, the same five object identities, the
`prod` environment and the existing deployment lock. It uses the same source,
retained-storage, backup, healthy-peer and operator guards, then submits exactly
the recovery's first conditional pause patch with `--dry-run=server`.

[Server-side dry-run](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run)
runs request validation and compatible admission without persisting the request.
The command returns immediately after that one request, even when it fails. It
does not await a pause acknowledgment, fence an instance, delete a Job or claim,
resume reconciliation or perform a cleanup write. It cannot combine with
execution, proof or continuation flags and cannot reuse a recovery confirmation.

A successful diagnostic proves only that its bound request was accepted at that
observation. It does not prove that the failed recovery had the same cause, that
a later write would pass, or that recovery is authorized. Publishing this
workflow does not authorize dispatching it; obtain separate diagnostic approval.

## Initial failed-Pod procedure

The procedure requires three observed database instances, a stable healthy primary,
one other healthy replica, healthy archiving, and a completed cluster-bound backup
no older than 24 hours. Current Cluster and failed Pod UIDs must be provided. The
operator must be the audited, fully rolled-out CloudNativePG 1.30.1. An unknown
read, different volume layout, active database Job, existing fence, changed
identity or changed primary stops the repair.

The failed replica's single existing volume is changed to `Retain`. The replica
is fenced, and its own metrics must acknowledge that PostgreSQL is fenced before
the claim is detached. The protected workflow reads the exporter over loopback
inside the exact failed Pod's `postgres` container, under a remote timeout and
with identity checks before and after the read. It still requires HTTP 200 and
exactly one positive fencing gauge; an annotation or log line is insufficient.
This avoids the Pod-proxy connection to port 9187 that the namespace's network
policies deny. No network policy or OIDC read permission is expanded. Detachment
follows the operator's
[`destroy --keep-pvc` procedure](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/internal/cmd/plugin/destroy/destroy.go):
remove only the verified Cluster owner reference and mark the claim `detached`
before deleting the exact failed Pod with UID and resource-version preconditions.
The operator's [fenced-candidate ordering](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/postgres/status.go)
keeps the fenced replica out of primary election. No PVC, PV, backup, primary,
healthy replica, disruption budget or namespace is deleted or relaxed.

The fence is removed only after the old Pod is absent and the same protected
peers remain healthy. Success requires two separated, complete samples showing
three Ready instances, fresh replacement storage, healthy archiving, and the old
claim and volume still bound to their original identities. Only then may ordinary
maintenance proceed. This proves database repair, not overall platform health.

Every write names the field manager `wedding-standby-repair`. Flux takes over
fields written by a default `kubectl` manager and removes what Git does not
declare, so a fence written that way is deleted at the next reconciliation and
the repair stops before the claim is detached.

## Read-only plan

From current main, get the current Cluster and failed Pod UIDs with OIDC reads,
then run:

```sh
go run ./scripts/replace-wedding-standby --cluster-uid "$CLUSTER_UID" --pod-uid "$POD_UID"
```

The command reports `PLAN=PASS no writes` only after complete observations pass.
The protected workflow requires confirmation
`retain-volumes-replace-failed-standby` and those same UIDs. It rechecks that its
validated checkout is current main before every conditional write. Never rerun
a failed dispatch: a new attempt requires fresh inspection and a new approval
for any continuation beyond the original procedure.

## Failure and retained data

A failure returns `REPAIR=HOLD`, never health clearance. Earlier successful
steps may remain: a Retain volume, a fenced failed replica, or a detached claim.
Rejected commands include only a bounded reason category: a recognized server
status, caller cancellation/deadline, or UNKNOWN. Raw stderr and command error
text are never published. Kubectl's native generic invalid-request response is
reported as `SERVER_INVALID`; Kubernetes has already discarded the underlying
patch error, so that category cannot identify a failed precondition or grant a retry.
A server status does not establish a particular
admission policy, failed JSON-patch test or transport cause; ambiguous, oversized
or unrecognized evidence stays UNKNOWN.
There is no automatic destructive cleanup, reverse ownership change, WAL reset,
or repeated mutation. Inspect the current state with OIDC reads and prepare a
reviewed continuation. Keep the old claim and volume for diagnosis; retirement
is a separately approved data-lifecycle operation.

The `resume_fenced` workflow input defaults to false. A specifically approved,
fresh dispatch can set it to true only for the pre-detachment HOLD: the exact
target fence must be owned by `wedding-standby-repair`, and the original claim
must still be attached and Bound to its retained volume. The procedure does not
rewrite that fence. Every other health, identity, backup and fencing-acknowledgment
gate still applies. Missing or foreign fence ownership, a detached claim, changed
identities, or a later partial repair remains HOLD and needs a separate reviewed
continuation. A read-only plan for this state uses `--resume-fenced` without
`--execute`; planning never executes a command inside a Pod.

`Verify Retained Wedding Fence` is a separate dispatch-only, read-only workflow
for the instance acknowledgment that local OIDC reads cannot reach. It requires
current identity guards, main, first-attempt confirmation
`prove-retained-wedding-fence`, the `prod` environment and the shared deployment
lock. Its fixed command reads only the exporter, and complete observations before
and after must preserve the original peers and retained storage. Inputs are
identity guards, never command or target selectors. It prints only a sanitized
PASS/HOLD result; it does not run the repair. A passing proof does not authorize
a continuation or declare the database recovered.

## Offline validation

```sh
go test -race -count=1 ./scripts/replace-wedding-standby
```

The workflow runs this package on its pull request, merge group and explicit
dispatch before any production credentials are restored.
