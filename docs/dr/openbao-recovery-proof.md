# Isolated credential recovery proof

The main-only verification workflow reads the fixed deployment facts and a fresh
off-cluster snapshot. Recovery credentials come from a completed Velero resource
backup at the same declared storage destination. It never reads the running
service's recovery-key Secret. Moving credential custody remains a separate
operation after the complete recovery proof passes.

The reader binds the backup namespace, resource coverage, completion window and
storage destination before downloading its exact archive with an immutable
conditional read. Archive size and expansion are bounded. Only the fixed recovery
resource is decoded in memory; nothing is extracted to the host filesystem.
Duplicate or conflicting resource copies, incomplete gzip data, bad checksums
and unrelated locations refuse success. Legacy custody-copy evidence retains its
specific completed snapshot-job window. Active canonical recovery instead reads
the newest independent snapshot from the complete bounded listing within the
last 24 hours, with at most five minutes of clock skew. It does not accept a
producer window from an input or fall back to an older matching snapshot. This
allows a completed backup to cover records created after the one-time copy's
snapshot. Every selected canonical record must still survive the actual restore;
a recent snapshot that lacks a copied record is not recovery evidence.

The verifier starts the pinned server release in a fresh, network-disabled
container. The container runs as the non-root runner user, with a read-only root
filesystem, no capabilities, no privilege escalation and bounded memory-backed
storage. The actual server image is pinned separately from the older CLI-only
image used by the scheduled snapshot client. A future server rollout requires
rebinding this recovery verifier to the approved running release.

Only a fixed loopback API is available. The verifier initializes its own temporary
Raft store, restores the supplied snapshot there, and requires the supplied root
token to recover every field in all three existing Email credential records.
The normal main-only workflow selects the three `infrastructure/email/`
records for both collection and restoration. It refuses missing or conflicting
canonical data rather than accepting a retained `apps/private-mail/` copy.
Explicit legacy modes remain for the guarded custody-copy operation and older
recovery evidence. Neither mode accepts an arbitrary credential path.
It checks that the restored server has exactly one local voting leader. It then
seals that disposable server, unseals it with the supplied recovery key and repeats
the state, membership and record checks. A prior automatic unseal cannot hide a
wrong recovery key.

Recovery keys and records travel through private files mounted read-only, never
through process arguments or container environment variables. The temporary
server receives no Kubernetes access, external network or host data directory.
Server output is discarded; the wrapper publishes only its fixed verdict
vocabulary and retains client diagnostics privately. Cleanup binds an owned
container to a random label before removing it, including a lost start response.
An unreadable cleanup result is explicitly unverified.

CI creates synthetic snapshots for both custody layouts with the actual pinned
server, archives their recovery credentials and removes the original credential
files. Every restore
obtains its credentials from the archive reader. It requires successful
restoration, rejects a corrupted snapshot, rejects an altered expected record,
rejects an archived wrong unseal key, and refuses expectations for the other
custody layout. Local tests without the explicit native test setting skip that
container exercise and are not evidence that a restore worked.

Do not point this program at production: it has no configurable API address.
Do not run production handoffs manually through this staging wrapper. The reviewed
main-only workflow must obtain the fixed inputs, enforce private RAM custody and
freshness, bind the running server version, and record the final production proof
before operational custody changes proceed.

The separate `Copy Email Credential Custody` workflow repeats this complete
independent recovery before copying any existing Email credential. It accepts
only a confirmed first-attempt dispatch from reviewed main and uses private
memory for the recovered credentials. The copy helper conditionally creates
only the three fixed infrastructure records, compares complete source and
destination values, rereads the sources, and refuses conflicts or incomplete
totals. It never deletes the legacy records or switches consumers. The existing
`Verify Email Credential Custody` workflow remains a read-only plan. Storage-key
generation, consumer switching and encrypted-volume activation are separate
operations after their own recovery coverage is verified.

Production explicitly uses retained credential custody: its overlay removes
the three Email PushSecrets while retaining the durable ExternalSecret readers.
Regenerated local caches therefore cannot create a missing canonical record
after an older backup is restored. Restore OpenBao first, then run the guarded
legacy custody copy when the restored backup predates the move. A conflicting
canonical value remains a refusal; do not delete history to make copying pass.

The fresh-install administration base still generates and seeds all three
accounts automatically with `IfNotExists` and retained remote history. Use that
fresh bootstrap bundle only for an explicitly declared new installation, never
while an existing installation awaits restoration. A fresh production bootstrap
must restore the retained-custody overlay before routine operation. Missing
records or regenerated caches alone never establish fresh-install intent.

## Preparing the Email storage key

`Prepare Email Storage Key` is a protected first-attempt, main-only operation,
confirmed with `prepare-email-storage-key`. Before creating anything, it repeats
an independent restore of the three canonical Email accounts and compares those complete values
with their fixed infrastructure destinations. The private connection files stay
outside the restore sandbox. A fresh key uses 256 bits of system randomness and
a create-only KV-v2 write. Existing valid keys are preserved. Deleted history,
conflicts, changed account values, uncertain writes and changed key readback
refuse success without retrying or replacing the key.

Its `EMAIL_STORAGE_KEY=PREPARED` receipt explicitly says `recovered=false`.
Preparation does not select or migrate an Email volume. The service key lives in
OpenBao, while the existing approved independent recovery credentials recover
OpenBao itself. No per-client certificate or new host-only recovery copy is
created.

After the configured off-cluster backup contains the new record, confirm
`verify-email-storage-key-recovery` in `Verify Email Storage Key Recovery`.
Its fixed collection and restoration modes include all three canonical accounts
and `infrastructure/email/storage`, whose sole field is the exact 64-character
hexadecimal passphrase. The normal three-account and legacy modes remain
separate. Native Linux tests restore all four synthetic records with the pinned
server and refuse an altered storage passphrase, corrupt snapshot, wrong unseal
key or wrong custody scope.

Only the actual independent result
`OPENBAO_BACKUP_RESTORE=PASS records=4 version=2.6.3` clears this key's recovery
prerequisite. A prepared key, a recent snapshot missing the key, a skipped native
test or an offline fixture is insufficient. Encrypted-volume canary, data
migration, restoration and activation remain separate reviewed operations; keep
the current mailbox volume until those checks and rollback are proven.

### Refresh the durable snapshot after custody changes

`Refresh Email Recovery Backup` is a confirmed, first-attempt main-only operation
with confirmation `refresh-email-recovery-backup`. It starts one run of the
current reviewed daily snapshot template, retaining its CronJob controller owner
and its existing service account, recovery volume and off-cluster mirror. It does
not change the schedule, retention, account credentials or storage key. An active
run, unexpected template, changed source, uncertain creation or replacement Job
refuses a completion claim; the operation never deletes uncertain resources.

Run it after the canonical credential copy and again after storage-key
preparation. A completed snapshot and mirror is still `restore=UNVERIFIED`:
follow it with `Verify OpenBao Backup Recovery` for the three canonical records,
or `Verify Email Storage Key Recovery` for all four records after key preparation.
Only the corresponding independent restore clears recovery. The temporary
backup uses the daily template's existing bounded deadline and normal CronJob
history management.
