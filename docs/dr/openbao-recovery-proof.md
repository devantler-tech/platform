# Isolated credential recovery proof

This tool prepares the restore portion of #5407. It does not download a production
backup, read production credentials, or authorize a custody migration. The
main-only read workflow and a fresh remote-backup observation remain separate
required delivery work.

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

CI creates a synthetic snapshot with the actual pinned server. It requires
successful restoration, rejects a corrupted snapshot, rejects an altered expected
record, and rejects a wrong unseal key. Local tests without the explicit native
test setting skip that container exercise and are not evidence that a restore
worked.

Do not point this program at production: it has no configurable API address.
Do not run production handoffs manually through this staging wrapper. The reviewed
main-only workflow must obtain the fixed inputs, enforce private RAM custody and
freshness, bind the running server version, and record the final production proof
before #5407 or #5376 can close.
