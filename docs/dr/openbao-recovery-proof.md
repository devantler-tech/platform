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
and unrelated locations refuse success. The data snapshot retains its separate
freshness requirement from the repaired snapshot job.

The verifier starts the pinned server release in a fresh, network-disabled
container. The container runs as the non-root runner user, with a read-only root
filesystem, no capabilities, no privilege escalation and bounded memory-backed
storage. The actual server image is pinned separately from the older CLI-only
image used by the scheduled snapshot client. A future server rollout requires
rebinding this recovery verifier to the approved running release.

Only a fixed loopback API is available. The verifier initializes its own temporary
Raft store, restores the supplied snapshot there, and requires the supplied root
token to recover every field in all three existing Email credential records.
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

CI creates a synthetic snapshot with the actual pinned server, archives its
recovery credentials and removes the original credential files. Every restore
obtains its credentials from the archive reader. It requires successful
restoration, rejects a corrupted snapshot, rejects an altered expected record,
and rejects an archived wrong unseal key. Local tests without the explicit native
test setting skip that container exercise and are not evidence that a restore worked.

Do not point this program at production: it has no configurable API address.
Do not run production handoffs manually through this staging wrapper. The reviewed
main-only workflow must obtain the fixed inputs, enforce private RAM custody and
freshness, bind the running server version, and record the final production proof
before #5513 can close or operational custody changes proceed.

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
