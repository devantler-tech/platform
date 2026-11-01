# Verify Email mailbox-data recovery

The confirmed `Verify Email Mailbox Data Recovery` workflow runs only from an
immutable main revision, on its first attempt, in the protected production
environment. It shares the production deployment lock. It does not move the
serving claim or activate encrypted production storage.

The driver verifies the serving pod, claim and backend identity, then imports one
uniquely identified diagnostic message into an unsubscribed test mailbox. JMAP
import does not submit an outgoing message. The current account credential stays
in runner memory and is never installed in the restored pod.

A fresh Velero filesystem backup includes only the selected Email pod and its data
volume. The restore uses the completed, source-bound Kopia snapshot as bytes;
backed-up pods, hooks, routes and credentials are not replayed. A fixed recovery
server reads a separate, three-replica encrypted volume with a new diagnostic key.
A single reviewed administration declaration supplies the exact runtime digest
for both source validation and the recovery reader. The declared binary base must
remain compatible with the native datastore proof; missing or changed facts stop
preflight before the diagnostic message is created.
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
