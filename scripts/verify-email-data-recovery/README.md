# Verify Email mailbox-data recovery

The separate `Dispose Verified Retained Email Copy` workflow targets only the
fixed retained diagnostic invocation. It requires the independent reader and
claim UID witnesses in the private `EMAIL_RETAINED_READER_UID` and
`EMAIL_RETAINED_CLAIM_UID` metadata settings. These are object identities, not
service credentials. Missing or replaced identities refuse disposal. No identity
value or mailbox content belongs in a public workflow input or receipt.

Before deleting anything, it verifies the reviewed workload, encrypted clone,
completed snapshot/transfer, backup retention, absence of transfer helpers, and a
complete metadata inventory across all served namespaced APIs. It requires one
exact diagnostic with matching account, mailbox, message, blob and content on
both source and retained copy, then repeats both reads. Source mailbox writes are
blocked at the HTTP boundary. All source messages and the backup remain intact.
Only UID-fenced test resources are removed; the key remains until both storage
backends are absent. Changed, foreign or uncertain evidence retains the copy.

This reports retained-copy readback and disposal only. The original trial's
random identity was not durably journaled, so its fresh-trial PASS cannot be
recreated. A fresh successful recovery exercise and production storage cutover
remain separate acceptance gates. Partial disposal reports an unverified cleanup
state and requires diagnosis; the workflow does not adopt replacements or
silently resume an uncertain delete.

The confirmed `Verify Email Mailbox Data Recovery` workflow runs only from an
immutable main revision, on its first attempt, in the protected production
environment. It shares the production deployment lock. It does not move the
serving claim or activate encrypted production storage.

The driver verifies the serving pod, claim and backend identity, then imports one
uniquely identified diagnostic message into an unsubscribed test mailbox. JMAP
import does not submit an outgoing message. The current account credential stays
in runner memory and is never installed in the restored pod.

A fresh Velero filesystem backup selects the Email pod and includes its related
claim, which the controller needs to capture persistent data. Scratch-volume
receipts may coexist; acceptance requires the exact data-volume receipt, not
the backup's overall Completed phase. Velero reads explicitly select its API
group so another installed Backup resource cannot shadow the recovery object.
The restore uses the completed, source-bound Kopia snapshot as bytes;
backed-up pods, hooks, routes and credentials are not replayed. A fixed recovery
server reads a separate, three-replica encrypted volume with a new diagnostic key.
A single reviewed administration declaration supplies the exact runtime digest
for both source validation and the recovery reader. The declared binary base must
remain compatible with the native datastore proof; missing or changed facts stop
preflight before the diagnostic message is created.
Source validation accepts the administration profile and its optional named
client proxy only when every component uses that reviewed bundle. Each component
must be ready with a runtime image identity and a valid restart count. Both are
bound across source-retention reads; API list ordering does not change identity.
A namespace policy denies external ingress and egress. The API client uses only
private Kubernetes port forwarding to IPv4 loopback and never follows redirects.

A successful run reads the exact message identity and content from both stores,
removes only its own source test message and folder with state/identity fences,
and verifies that the serving claim and backend remain unchanged. It then removes
the restored pod and claim, observes both its PV and storage backend absent, and
only afterward removes the diagnostic key and namespace. Created resource UIDs
fence each destructive request; unknown namespace workloads or claims block
namespace disposal. The completed backup deliberately retains its normal
24-hour expiry as a fresh recovery point.

Cleanup recognizes only the four platform-generated network policies whose
specs match this checkout and whose Kyverno labels name the created namespace
UID and expected rule. Standard and Cilium policies are both inventoried;
unknown policies or changed specs retain the namespace and recovery key.

Any failed, ambiguous or interrupted operation is unverified. The driver retains
uncertain resources and keys for inspection instead of retrying, adopting a
replacement or deleting by name. Source markers can remain after a failure.
Use the workflow run ID to identify that invocation's diagnostic namespace and
backup, inspect controller completion and resource UIDs, and establish safe
cleanup before removing retained resources. Do not delete storage keys while a
restore operation or encrypted backend may still need them. A workflow rerun is
refused; diagnose before a new confirmed dispatch.

The confirmed `Observe Retained Email Recovery` workflow performs a read-only
diagnosis of invocation `41561215415`. Its target is fixed in reviewed code. It
binds the namespace, reader UID and declared workload, samples readiness without
authentication, and only then reads the existing account credential to check a
JMAP session. It performs no mailbox write, new restore or cleanup. A successful
observation does not establish message recovery. Its output contains only a
status code and closed stage names. Future recovery failures distinguish reader
forwarding, readiness, session and message checks instead of grouping them.

With confirmation `verify-retained-email-parity`, the same workflow also binds
the configured original reader and claim UIDs and compares the diagnostic
message with the reviewed serving workload. Its read-only JMAP client checks
both copies twice. A missing or expired original backup does not prevent this
comparison. `EMAIL_RETAINED_PARITY=VERIFIED` reports present message parity.
The original trial stays unverified and disposal still requires its own backup
and ownership checks.

The separately confirmed `Prepare Retained Email Reader Inputs` workflow
prepares custody for the original retained reader. It runs only on the first
maintainer-dispatched attempt from current reviewed main, under the production
deployment lock. It binds the independently recorded reader and claim, reads
the exact retained encryption key, recovery authentication, configuration and
declared workload inputs, and rechecks their identities and contents immediately
before the one conditional OpenBao write. Kubernetes access is read-only.

The destination is a create-only, original-version KV2 record under
`infrastructure/email/recovery/retained-reader-inputs`. The command uses CAS=0;
an identical existing record is read back without rewriting it. The command
never updates or deletes the record. Changed,
deleted, newer, malformed or unreadable records refuse preparation. A failed
or lost write response is uncertain and is not retried. Original inputs remain
in place on every outcome. Neither credentials nor resource identities are
printed; the short-lived credential files exist only in runner memory.

`EMAIL_READER_INPUT_CUSTODY=STORED` proves the observed record matches those
retained inputs. Its receipt explicitly says `restart=NOT_AUTHORIZED` and
`recovery=UNVERIFIED`: independent recovery of the stored inputs, a durable
restart witness, the separate protected pause/resume operations and native
detachment proof remain required. This workflow never stops the reader,
allocates storage, changes the source mailbox or grants disposal authority.
The expired-backup disposal gate remains unchanged. If custody is refused,
investigate the retained identities and destination before another confirmed
first attempt; never replace a conflicting record or regenerate a missing key.

`Protect Retained Email Volume` is a separate, default-off main-only operation
with confirmation `protect-retained-email-volume`. It requires independent
reader, claim and PV UID witnesses, including the private
`EMAIL_RETAINED_PV_UID` setting, and validates the actual restored binding.
It deliberately does not assume that the original PV name contains the current
restored claim's UID. The command preserves the running reader and every
claim, backend, key and configuration object. Its only permitted mutation is
changing the bound original PV's reclaim policy from `Delete` to `Retain`,
with atomic UID, name, resource-version and complete-spec tests. It then
rechecks the original graph and serving source. An already-retained PV is
verified without a write; an uncertain patch is never retried or rolled back.
No credential export or reader lifecycle authority is available in this
command. Protection does not unlock either expired-backup disposal path,
establish key recovery or authorize a later reader pause.

With confirmation `verify-fresh-retained-email-parity`, the workflow checks
the separately retained fresh restore. The protected witness binds the
namespace, reader, claim, backup and download. The backup must still be
unexpired and the completed transfer must match its mailbox snapshot and the
encrypted scratch claim. The observer compares the one diagnostic from the
protected run's creation window on both copies. Its receipt reports message
parity and leaves disposal to a separately reviewed operation.

The pass receipt covers message recovery, encrypted disposable storage, source
retention and disposal of the restored copy. It does not establish an independent
cluster rebuild, archive migration, transport delivery, OIDC, or production
storage activation. Those retain their separate acceptance gates.

Local validation is `go test -race ./scripts/verify-email-data-recovery`, `go vet`
and the package's normal lint check. The native Email configuration fixture also
runs the real driver client against the pinned server to exercise message
import, content validation and safe cleanup. Production recovery remains
unverified until the confirmed protected workflow itself passes.
The native restored server also uses the reader's generated capability settings,
proving that the pinned executable can start with its required file capability.

To evaluate only the source guard with an existing read-only Kubernetes context,
set `EMAIL_RECOVERY_READONLY_SOURCE_CONTEXT` and run
`go test -run '^TestServingSourceLiveReadOnly$' -count=1 ./scripts/verify-email-data-recovery`
from the repository root. This opt-in reads only the fixed serving Pod, claim
and volume twice. It does not invoke recovery, import a message or create any
resources, and is skipped during normal tests.

To check native backup-resource selection after a retained trial, also set
`EMAIL_RECOVERY_READONLY_BACKUP_NAME` to that trial's
`email-data-recovery-<run>-backup` name and run
`go test -run '^TestReadOnlyVeleroBackupAPISelection$' -count=1 -v ./scripts/verify-email-data-recovery`.
It reads only the selected Velero backup and its volume receipts. It does not
establish a complete datastore backup or restore result.
