# Email storage encryption

The `email-encrypted-storage` bundle declares a retained, encrypted Longhorn
volume and its operational-key binding. Production serves the mail store from it
(#5387). The original, unencrypted claim is unchanged and stays as the rollback.

The key comes up before the server switches. The bundle was deployed on its own
first, and the cutover starts only once the `email-storage` SecretStore is valid
and the `email-storage-key` ExternalSecret has synced in production. The first
cutover attempt switched the server and brought up the key in one deployment.
The key never arrived, so the server could not start until its pod was replaced
on the original claim.

Longhorn uses dm-crypt/LUKS beneath the mounted filesystem. The Email process and
ordinary mail clients continue to read ordinary messages. This protects stored
volume contents; it does not protect against a running server, a cluster
administrator, memory access or interception outside the service's TLS boundary.
Application-level backups require their own encryption and recovery proof.

The operational passphrase resolves from the declared Email record in OpenBao
through a namespaced store and a dedicated role. That role is bound only to the
existing Email service identity and reads only this key record, with no account
credential access, writes, metadata enumeration or default policy. Without the
default policy, the role's policy itself grants the token lookup that External
Secrets uses to validate the store, and the token's own revocation. The shared
External Secrets role gains no permission.
The bundle never generates a replacement when that record is missing. External
Secrets materializes the CSI-required Kubernetes Secret, retained independently
of its controller object and immutable once created. A changed source value
therefore reports an error instead of replacing the in-use key. The Email
container receives no key mount. The three
node-side references cover initial staging, publishing and filesystem expansion;
the class adds no controller-side secret reference. A missing or wrong key can
prevent a detached volume from mounting again. Retention is not a backup, and
changing the stored passphrase is not a supported rotation procedure.

OpenBao recovery must work independently of this cluster. The approved recovery
credential in synchronized iCloud Keychain protects that independent recovery
path; a key stored only in OpenBao would not resolve an OpenBao bootstrap loss.
The existing backup restore must also be extended to compare the new storage-key
record before it can count as recovery evidence for encrypted Email data.

The steps before the cutover:

1. Preserve the current mailbox claim, with a current daily backup as a second
   safety net.
2. Establish the operational key without overwriting a conflicting value, then
   restore it from independent custody and compare it privately (#5540).
3. Run an isolated encrypted-volume canary on the deployed Longhorn/Talos
   versions. Run 40874287889 (2026-10-29) observed the encrypted volume, ordinary
   write and read, detach and remount, refusal with a missing or wrong key, and
   recovery, then verified its cleanup.
4. The copy-based cutover below. Never try to encrypt the old volume by changing
   its immutable storage-class fields.

## Copy-based cutover

The maintainer chose a simple copy over a full restore test before the cutover
(2026-11-14), which closed #5578 as a gate. A backup restore proof from encrypted
storage is a separate, non-blocking task (#6256).

Production's Email Pod mounts the encrypted claim as its data volume and the
original claim read-only. Its first initializer copies the original store once,
while the StatefulSet has stopped the previous server, then compares every entry's
type, mode and path and every file's checksum with the original. Only a matching
copy gets a completion marker; a mismatch, a missing store or a copy error stops
the Pod with the original untouched, and an unmarked copy is cleared and redone
on the next start. Later starts find the marker and skip the copy. The setup then
reads the accounts and settings back from the copied store before serving.

To roll back, revert the cutover: the Pod serves the original claim again. Mail,
calendar and contact changes made after the cutover exist only on the encrypted
volume. The original volume stays declared, prune-protected and mounted read-only
while the cutover is in place, so it cannot be deleted while in use; switching
its volume to `Retain` is part of #4441.

A rollback right after a refused copy or a failed start loses nothing: the
encrypted store has served no changes. A rollback after it has served is
different. No supported tool here merges two Stalwart stores, so the original
is the source of truth after any rollback. Every change the encrypted store
served before the rollback is not preserved: new, edited and deleted messages,
calendar entries and contacts. Weigh that loss before rolling back from a store
that has served.

After a rollback, the two stores diverge. Never reapply the cutover as it is:
its completion marker skips the copy, so the encrypted store would serve again
and hide every change made on the original during the rollback. Reapplying needs
its own reviewed change that takes a fresh backup of the original and then
empties the encrypted claim, marker included, so the next cutover copies the
original again. That review decides explicitly that the encrypted store's
changes are discarded, or adds a separately verified way to carry them over.

The existing `private-mail` namespace remains retained by its established storage
bundle. The replacement class is not the cluster default. Both the replacement
claim and its resulting PV retain data if their serving configuration is removed.
Cleanup requires a separate, verified retention decision.

References: [Longhorn 1.13 volume encryption](https://longhorn.io/docs/1.13.0/advanced-resources/security/volume-encryption/),
[Longhorn storage-class parameters](https://longhorn.io/docs/1.13.0/references/storage-class-parameters/).

## Isolated native storage canary

`Verify Email Storage Canary` is a manual, first-attempt, explicitly confirmed
workflow on reviewed `main`, protected by the production environment and the
production mutation lock. Its driver refuses other dispatch contexts before
reading the cluster. It checks Longhorn 1.13.0 and Talos 1.13.12; a version change
requires reviewing that binding before another run.

The workflow creates one invocation-owned namespace, a default-deny network
policy, a separate StorageClass, one 1 GiB test claim with three replicas, and a
random test-only key. It uses the production encryption parameters with its own
node-side key references. Its test class deliberately uses `Delete`, so cleanup
can remove the synthetic volume. The retained Email class remains `Retain`.
No serving claim, OpenBao record or operational encryption key is selected.

Use `verify-email-storage-canary` for the V1 canary or
`verify-email-storage-v2-canary` for the V2 canary. Before the V2 run, verify
three registered raw disks have capacity for the disposable claim. The V2 proof
requires the enabled data engine and three running replicas of the same
encrypted volume on distinct nodes and disks.

The test writes ordinary synthetic content, proves the actual CSI claim and
Longhorn encrypted-volume identities, and reads the same content after detach
and remount. It then removes the test key and requires a specific missing-key
mount error on the current pod. A different random key must produce the native
incorrect-passphrase error. A generic pending pod is insufficient evidence, and
the pod must remain unstarted throughout a 30-second refusal observation.
Restoring the original test key must recover the original content.

All resources are created with unique run names, without adopting existing
objects. Deletion uses a server-side UID precondition and must be followed by
observed absence. Cleanup waits for the test PV and Longhorn volume to disappear
before removing the key and namespace. An uncertain creation, replaced resource
or failed cleanup produces `UNVERIFIED`, retaining identity rather than guessing.
Do not rerun the same invocation to clean up: investigate its reported run ID
through a separately reviewed, ownership-bound cleanup when necessary.

A `PASS` receipt requires all native phases and verified cleanup. It proves the
storage mechanism on this deployed stack. It does not prove recovery of the
operational Email key or restore a company mailbox; those are covered above
(#5540) and by #6256.
