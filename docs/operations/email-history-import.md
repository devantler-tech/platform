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
| `destination-password` | An app password for the destination mailbox. The account has two-factor sign-in, so its own password cannot sign in over IMAP. |
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
importer. The server's own aliases come from OpenBao
`infrastructure/email/aliases`. The destination sign-in is an app password the
maintainer created for this copy and keeps at
`infrastructure/email/history-import` (key `password`). The same role reads it,
and the ExternalSecret projects it as `destination-password`. Revoke the app
password once the copy after the MX move is complete.

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

A failed folder check reports which step refused, as
`MAIL_HISTORY_IMPORT=INCOMPLETE stage=<side>-<step>`. The side is `source` (the
existing provider) or `destination` (this server). The step is one of a fixed
list, and no server reply, folder name or credential ever appears:

| Step | Meaning |
| --- | --- |
| `connect` | No TCP connection: name resolution or the network policy. |
| `tls` | The handshake failed: certificate trust, host name or a stalled peer. |
| `greeting` | The server's IMAP greeting was unusable. |
| `login` | The server refused the sign-in: check the username (for iCloud, the `@icloud.com` address) and the app-specific password. |
| `list` | The folder listing failed. |
| `delimiter` | A folder uses a hierarchy separator other than `/`. |
| `folders` | An empty, oversized, duplicate or malformed folder list. |
| `examine` | A read-only folder open failed, or its reply gave no message count (`* N EXISTS`), which would otherwise count the folder as empty. |
| `read-only` | Destination only: the read-only open (EXAMINE) was not confirmed with `[READ-ONLY]`, or was claimed read-write in a tagged or untagged reply. The destination is this deployment's own server, which sends the code. The source's code is not checked: iCloud answers EXAMINE with a code other than `[READ-ONLY]`, and the observer sends only read commands. |
| `uids` | A folder lacks UIDNEXT or UIDVALIDITY. |
| `inbox` | No INBOX was listed. |

### Pass lines

Each of the two copy passes also writes one line, whether it succeeded or not.
The first pass copies and the second, `--dry`, only verifies:

`MAIL_HISTORY_IMPORT_PASS pass=<copy|verify> exit=<n> observed=<n> identified=<n> errors=<n> transferred=<n> would_transfer=<n> skipped=<n> h1_duplicates=<n> h2_duplicates=<n> h2_crossduplicates=<n> h1_noheader=<n> h2_noheader=<n> in1_not_in2=<n> in2_not_in1=<n>`

The line holds numbers only.
- **Source of the counts.** The labelled counts come from a fixed list of labels, read only from imapsync's closing statistics, the block that starts with `++++ Statistics`. The first value of each label there is kept.
- **`errors` and `identified`** come from the result lines the success check already reads (`Detected N errors`, `The sync looks good …`, `The sync is not finished …`), wherever they appear. The last one wins.
- **Privacy:** no folder name, header, subject or server reply is read into the line.
- **`?`** is a value the tool did not print.
- **`observed`** is the source total that the pass is judged against.
- **`identified`** is the number of distinct message identities. A message filed in two folders counts once.
- **`exit`** is imapsync's exit status, with two special values:
  - `signal`: the process ended by a signal. That includes the import's 3-hour deadline, and also a SIGTERM or SIGINT that imapsync caught, because it then kills itself with the same signal.
  - `?`: the tool never ran or left no exit status, for example when the deadline had already passed.
- **A pass can fail with `exit=0`** when its output broke the importer's bounds of 128 MiB in total or 64 KiB per line. The status is kept even then.

Exit statuses in imapsync 2.314:
- **A run with errors**, whether it stops at `--errorsmax` or reaches its end, exits with the code of its most common error type:
  - 113 over quota, 114 append, 115 fetch, 116 create, 117 select;
  - 118 transfer limit, 119 append refused as a virus, 120 flags, 121 search;
  - 111 when the type is unclassified.
  112 is defined but neither path uses it.
- **Connection and sign-in:** 10 (connection), 101 and 102 (connection to the source or the destination), 12 (TLS), 16, 161 and 162 (authentication; 161 and 162 are the source and destination users).
- **Other stops:** 6 when imapsync caught a signal and was still running after killing itself (normally it reads `signal`), 7 for an abort file, 8 for a pid file error, 1 for any other failure.

A pass line never means
success: only the `COMPLETE` receipt does. To read a failure:

| What the line shows | Likely cause |
| --- | --- |
| `pass=copy` with `errors` above 0 | An error stopped the pass, and `--errorsmax 1` stops it at the first one. `errors` counts every type of error, including fetch, append, create, select, flags, quota, TLS and connection errors. `exit` names the type; see the list above. |
| `pass=verify` with `identified` above `observed`, and `in1_not_in2` equal to the difference | Mail reached the source during the run. Repeat at a quieter time. |
| `identified` below `observed`, with `in1_not_in2` at 0 | The source holds the same message in more than one folder, or a folder holds duplicates. imapsync counts each identity once, so a pass that copied everything still cannot meet `identified = observed`, and repeating cannot change it. This needs a change to the importer's success rule, not another run. |
| `in2_not_in1` above 0 | Either mail the destination held before the copy, which the importer keeps, or copies stored with a different identity (`Message-Id` and `Received` headers). Compare runs before deciding. |
| `h2_duplicates` rising between runs, with `in1_not_in2` above 0 in each verify pass | Copies were stored with a different identity, so each run appends them again. Stop repeating the operation, because every repeat adds duplicates, and look at what the destination changed. `in2_not_in1` counts distinct identities, so it can stay the same while the copies pile up, and a stable value does not rule this out. |
| `pass=verify` with `in1_not_in2` above 0, `h2_duplicates` unchanged between runs, and the source unchanged | Messages left the destination after they were copied, for example through its retention of old Trash or Junk messages. |

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
   next UID on the source and destination, using `EXAMINE`, the read-only open.
   The destination must confirm each open as read-only; the source's reply code
   is not checked (iCloud does not confirm it).
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
`CAPABILITY`, `LOGIN`, `LIST`, `EXAMINE` and `LOGOUT`; it neither fetches bodies
nor sends mailbox mutation commands, and the tests refuse any other command.
iCloud answers `EXAMINE` with `[READ-WRITE]`, so the observer's opens of the
source may clear its session `\Recent` flags; the copy's source checks cover
folders, message counts, UID validity and next UID, not flags. Its connections
have an absolute deadline, including while waiting for a server greeting.
