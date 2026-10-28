# Email storage encryption preparation

The optional `email-encrypted-storage` bundle declares a new retained Longhorn
volume and its operational-key binding. Production does not select this bundle,
and the existing mailbox claim is unchanged. This prepares part of #5387; it
does not establish encrypted production storage or a completed migration.

Longhorn uses dm-crypt/LUKS beneath the mounted filesystem. The Email process and
ordinary mail clients continue to read ordinary messages. This protects stored
volume contents; it does not protect against a running server, a cluster
administrator, memory access or interception outside the service's TLS boundary.
Application-level backups require their own encryption and recovery proof.

The operational passphrase resolves from the declared Email record in OpenBao
through a namespaced store and a dedicated role. That role is bound only to the
existing Email service identity and reads only this key record, with no account
credential access, writes, metadata enumeration or default policy. The shared
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

Before selecting the bundle or moving data:

1. Preserve the current mailbox claim and obtain a verified current backup.
2. Establish the operational key without overwriting a conflicting value, then
   restore it from independent custody and compare it privately.
3. Run an isolated encrypted-volume canary on the deployed Longhorn/Talos
   versions. Observe the actual encrypted volume, ordinary write/read, detach and
   remount, refusal with an unavailable or wrong key, and successful recovery.
4. Prepare and review the quiesced copy and explicit workload-claim cutover.
   Verify mailbox history, new delivery, backup restoration and rollback while
   retaining the original claim. Never try to encrypt the old volume by changing
   its immutable storage-class fields.

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
reading the cluster. It checks Longhorn 1.13.0 and Talos 1.13.11; a version change
requires reviewing that binding before another run.

The workflow creates one invocation-owned namespace, a default-deny network
policy, a separate StorageClass, one 1 GiB test claim with three replicas, and a
random test-only key. It uses the production encryption parameters with its own
node-side key references. Its test class deliberately uses `Delete`, so cleanup
can remove the synthetic volume. The retained Email class remains `Retain`.
No serving claim, OpenBao record or operational encryption key is selected.

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
operational Email key, restore a company mailbox, or enable encrypted production
storage; those remain separate acceptance gates above.
