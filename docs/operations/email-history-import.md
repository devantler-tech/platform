# Copying existing mailbox history

The history importer prepares the company mailbox for a move from iCloud. It
uses imapsync to stream folders and messages directly to the destination, so
there is no second archive of private correspondence to store or dispose of.
The source provider keeps its copy.

## Published tool

`Publish Email History Importer` tests, builds and signs
`ghcr.io/devantler-tech/platform-email-history-import` from reviewed `main`.
The image contains the repository's Go driver and imapsync 2.314, fetched from
upstream commit `8895966244fb161cc68f2c8427a8f6326a7f2ab0` with a SHA-256 check.
Its base image is pinned too. The imapsync licence is included in the image.

Use the published **digest**, after verifying the signature's issuer and exact
main-branch publisher identity. Publication prepares a tool; a separate reviewed
operation must arrange its short-lived secret mounts and network access before
it can copy real mail. Never run the importer on a contributor's machine with
real credentials.

## Private inputs

Mount these files read-only in one private input directory:

| File | Value |
| --- | --- |
| `source-username` | The existing provider's account login. |
| `source-password` | A temporary, app-specific source password. |
| `destination-password` | The existing destination mailbox password. |
| `addresses` | The complete approved domain-address inventory, separated by commas. |

The driver rejects missing, empty, multiline or publicly readable credentials,
projected symlinks escaping the input directory, duplicate or foreign-domain
addresses, and an inventory missing the destination address. These are input
checks: the maintainer must still establish that the inventory is complete and
that every listed address reaches its intended destination before MX changes.

`history --input-dir /private/inputs --check` validates the mounted inputs without
connecting to either server. It prints only a fixed result and address count.
The copy command requires the separate literal confirmation
`--confirm copy-icloud-history`.

## Running the copy in production

The maintainer keeps the existing provider's `username` and app-specific
`password` in OpenBao at `infrastructure/apple/icloud`, which only the
`email-history-import` role can read. The `email-history-import` ExternalSecret
in `private-mail` projects them as `source-username` and `source-password`.
The domain's address inventory is not a provider credential: it is the
SOPS-encrypted `email-history-import-addresses` Secret, removed with the
importer. The server's own aliases stay declared in the encrypted mail
settings. The destination password comes from the existing
`private-mail-maintainer` Secret.

The suspended `private-mail/email-history-import` CronJob holds the reviewed
pod template: the signed importer digest, those inputs mounted read-only, and a
network policy that reaches only the two fixed IMAPS endpoints. It never runs
on a schedule. To copy:

1. Check that the ExternalSecret is `SecretSynced`. A wrong key name shows up
   in its status without revealing any value.
2. Run **Start Email History Import** with confirmation
   `start-email-history-import`. It creates one Job from the template and
   returns; see [its README](../../scripts/start-email-history-import/README.md).
3. Follow the Job read-only. Its log holds only the fixed result lines below.
   `MAIL_HISTORY_IMPORT=COMPLETE folders=<n> messages=<n> source=PRESERVED routing=UNCHANGED`
   is the receipt to record on #5481.

A Job created this way is outside the Flux inventory, so a long copy never holds
a deploy and a reconciliation cannot prune it. Repeat the operation to continue
an incomplete copy; it refuses while an earlier copy is still running.

## Copy and reconciliation

The driver contacts only the fixed source and destination IMAPS endpoints on
port 993. It requires TLS 1.2 or later, a trusted certificate authority and the
correct hostname before authenticating. Passwords are read from the mounted
files and are never placed in subprocess arguments.

The operation proceeds in this order:

1. Independently read every selectable folder's message count, UID validity and
   next UID on the source and destination, using read-only `EXAMINE` commands.
2. Copy all folders and messages with imapsync. Source and destination expunges,
   deletion, ACL changes and updates of existing destination flags are disabled.
   A copied message's pending-deletion flag is removed so it remains readable.
3. Run imapsync again in dry-run mode. Both passes must report no missing
   identified messages, no unidentified source messages and zero errors.
4. Independently read the folders again. Every source folder must exist on the
   destination with at least its source message count; existing destination
   folders and message counts must be preserved. The source's complete folder
   inventory, message counts, UID validity and next UID must be unchanged.

The driver refuses unsupported folder separators, more than 1,000 selectable
folders, unknown UIDs, interrupted observations, and incomplete tool reports.
Connections have finite deadlines and the operation has a three-hour limit.

Only after all checks pass does it print an aggregate completion receipt with
folder and message counts. Folder names, addresses, credentials, message headers
and upstream tool output never enter the public receipt. The source copy is
retained after failures too. An interrupted or changing source is incomplete;
repeat the confirmed operation to finish the incremental copy.

The tool matches messages using imapsync's existing header comparison, with
duplicate copying enabled. It refuses completion if upstream cannot identify a
message. The folder readback checks counts; it does not replace the separate
private review of representative content and the complete address inventory.
Moving MX, configuring mail apps and proving external delivery remain separate
acceptance steps.

## Upstream assessment

Imapsync is maintained by Gilles Lamiral and widely used for incremental IMAP
migrations. Its [source](https://github.com/imapsync/imapsync) and
[licence](https://github.com/imapsync/imapsync/blob/8895966244fb161cc68f2c8427a8f6326a7f2ab0/LICENSE)
permit this use. The pinned source was checked for source-side writes, body
`PEEK` reads, deletion defaults, credential-file handling and TLS options.
The driver overrides the upstream defaults for certificate verification,
external release checks, logs and deletion behavior. No hosted migration service
receives the mailbox data.

The independent inventory client is the MIT-licensed
[`go-imap` v1.2.1](https://github.com/emersion/go-imap/tree/v1.2.1). It sends only
login, folder listing, read-only selection and logout; it neither fetches bodies
nor sends mailbox mutation commands. Its connections have an absolute deadline,
including while waiting for a server greeting.
