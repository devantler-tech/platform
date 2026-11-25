# Verify Email mailbox-data recovery

The confirmed `Verify Email Mailbox Data Recovery` workflow runs only from an
immutable main revision, on its first attempt, in the protected production
environment. It shares the production deployment lock. It does not move the
serving claim.

The server serves its store from the encrypted claim `email-data-encrypted`
(storage class `email-encrypted`); the original `private-mail-data` claim stays
mounted read-only as the rollback and is not the store this proof restores.
The driver verifies the serving pod, claim and backend identity, then imports one
uniquely identified diagnostic message into an unsubscribed test mailbox. JMAP
import does not submit an outgoing message. The current account credential stays
in runner memory and is never installed in the restored pod.

The driver also takes a census of the account's store before and after the
backup and again from the restored copy: mailboxes, messages, calendars with
their events, tasks, and account settings (sending identities, Sieve filter
scripts and the vacation response). It reads identities, counts and object
types only, never content, and keeps the counts in memory. An unchanged source
requires equal restored counts; a source that changed while the backup ran
bounds each restored count by its two readings. Reading calendars and
identities makes the server create its default calendar and identity if the
account has none; the first census runs before the backup, so the restore must
hold them too. The receipt names each category `VERIFIED`, or `EMPTY` where
the source held none (for example, no tasks), and never a count.

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
It also recognizes what the platform adds to every namespace: Kyverno's
`default-limitrange` and `default-resourcequota`, bound by their generate
labels to the created namespace UID and to the `add-default-limitrange` and
`add-ns-quota` rules, and trust-manager's `cloudflare-origin-ca` ConfigMap,
bound to its controlling Bundle's UID with its single CA key.

Before every disposal step that could remove the namespace, the driver also
reads its complete inventory: every namespaced, deletable resource of every
served API version, metadata only, through the dispatch's private credential.
Each object must be one this invocation created, with its created UID, or one
of the platform objects above. Runtime records about those objects are
accepted by exact kind and served resource: Events, Kyverno policy and
ephemeral reports named after a known object's UID, and Cilium's endpoint for
the created reader. Kubescape's storage is a remote aggregated API; only its
exact `v1beta1.spdx.softwarecomposition.kubescape.io` registration, backed by
`kubescape/storage`, and its workload scan-record kinds are accepted, and only
for this invocation's namespace. Any other object, kind, registration or failed
read retains the namespace and its key. Without the private credential no
recovery starts.

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
The original trial stays unverified.

With confirmation `verify-fresh-retained-email-parity`, the workflow checks
the separately retained fresh restore. The protected witness binds the
namespace, reader, claim, backup and download. The backup must still be
unexpired and the completed transfer must match its mailbox snapshot and the
encrypted scratch claim. The observer compares the one diagnostic from the
protected run's creation window on both copies. Its receipt reports message
parity.

The pass receipt covers message recovery, the store census, encrypted disposable
storage, source retention and disposal of the restored copy. It does not
establish an independent cluster rebuild, archive migration, transport delivery
or OIDC. Those retain their separate acceptance gates.

The backup is file level: Velero's node agent reads the mounted, already
decrypted store and Kopia encrypts it into the backup bucket under the Velero
repository password. Restoring it needs that password and read access to the
bucket, not the serving volume's Longhorn key (`email-storage-key`); the copy is
written to a new volume encrypted with a new diagnostic key. The in-cluster
restore uses the running cluster's repository credential, so this workflow does
not show that an independently held copy of that password opens the backup.

Local validation is `go test -race ./scripts/verify-email-data-recovery`, `go vet`
and the package's normal lint check. The native Email configuration fixture also
runs the real driver client against the pinned server to exercise message
import, content validation, the store census and safe cleanup. Production
recovery remains unverified until the confirmed protected workflow itself passes.
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
