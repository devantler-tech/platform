# Prepared alert publisher

This command prepares the publisher required by Platform #5291 and #4683. It is
disabled by default. Its first reviewed route is one aggregate CNPG health
observation owned by the public Platform repository. Further detector routes
require explicit ownership and public-message review.

The caller supplies one JSON observation on standard input:

```json
{"route":"cnpg-degraded","state":"firing","observedAt":"2026-10-24T20:00:00Z","complete":false}
```

States are `firing`, `healthy`, and `unknown`. A healthy observation requires
`complete: true` after a complete successful detector scan. The caller must
authenticate its source and establish that scan before invoking the command.
An expired signal or failed scan uses `unknown`. Unknown fields, routes and
states are refused without echoing their contents. The input contains no public
body, repository, resource identity, diagnostic, URL or destination.

## Publication boundary

The command's reviewed App and immutable repository identities are fixed in the
source. Its maintained App-token transport requests only the Platform repository
and Issues write, and validates the returned repository and permission grants.
Metadata read is GitHub's accompanying required grant. Native repository reads
check exact identity, visibility, archival state and issue availability before
writes. Ordinary user tokens are not accepted as credentials.

The App private key is a projected file supplied by the existing owner-controlled
credential procedure. It is never a command-line value, observation field or log
value. The command does not install an App or arrange a credential copy.

Confine the key to the reviewed publisher workload. Detectors, tenant code and
untrusted branch execution receive no copy of it. Deployment must preserve that
boundary, private durable state and constrained GitHub egress.

Explicit activation uses these operator-supplied facts:

```sh
alert-publisher --enabled --state-dir /state/alert-publisher \
  --key-file /credentials/app.pem --installation-id INSTALLATION_ID
```

`--drain` processes persisted work without reading a new observation. Without
`--enabled`, the command accesses neither credentials nor the network. No
deployment, route, scheduler or existing notification consumer invokes this
prepared command yet.

## Durable handling

The state directory must live on durable storage, have private permissions, and
be shared by every process publishing this route. A single filesystem lock
serializes writers. An atomic, synchronized journal records the intended
operation before network access. Runner-local temporary storage is unsuitable
for cross-run state.

The journal bounds pending work at 64 operations and 128 KiB, write attempts at
12 per UTC day and six per operation, and uncertainty at seven days. Confirmed
operation receipts are retained for eight days, up to 128 records. Capacity
failure refuses the observation rather than acknowledging work it did not save.

A POST timeout or server error leaves an unconfirmed operation. Later runs read
bounded, paginated history for its exact marker and verified bot actor. They
never automatically repeat an unproven write. Multiple matches, incomplete
history, changed identity or expired uncertainty require operator reconciliation.
Authentication and rate-limit refusals retain work with a bounded retry time.

An initial actionable message mentions the maintainer once. Repeated observations
share the issue. Recovery and loss of signal add fixed messages while retaining
open remediation. A later transition into firing reuses an open issue and creates
a new issue when the earlier issue is closed. This includes `unknown` to
`firing`: closure never establishes a healthy observation, and a newly confirmed
actionable condition still needs tracking. The command never closes a remediation
issue.

Exit 0 means processing completed, including an initial healthy or duplicate
observation that required no publication. Exit 2 means invalid input, deployment
facts or storage; exit 3 retains deferred or unconfirmed work; exit 4 requires
operator reconciliation. Only journaled object receipts prove GitHub delivery.
Inbox and mobile delivery require the separate controlled proof in #2755.

All existing alert routes, Alertmanager behavior and external heartbeat coverage
remain active until their replacement is reviewed and demonstrated route by
route. App or shared-credential retirement follows verified consumer migration.

## Cluster-independent journal preparation

The optional `r2` backend prepares the independent journal needed by #5328 and
#4683. It uses the maintained AWS S3 SDK with explicit static credentials,
the configured account's global Cloudflare R2 S3 endpoint, region `auto`,
bounded requests, no redirects and no automatic request retries. It does not
discover credentials from the environment or create a bucket. The existing
declarative Cloudflare owner provisions storage; no browser writes are needed.

Two private projected JSON files supply every storage fact. Configuration has
exactly `accountId`, `bucket`, `key` and `instance`. The account ID is 32 lower-case
hex characters; the bucket is a 3–63 character lower-case bucket name without
dots. The object key is a lower-case relative path ending in `.json`, using
letters, digits, slashes, underscores and hyphens. The explicit instance name
uses 1–64 lower-case letters, digits and hyphens, starting with a letter or digit.
Credentials have exactly `accessKeyId` and `secretAccessKey`. Both files are
bounded at 8 KiB, regular, and inaccessible to other users; write permission for
the projected credential's group is refused. Unknown, duplicate, missing or
malformed facts are refused without echoing them. Operator facts never come
from the observation payload.

Initialize an absent journal **once**, without any GitHub credential:

```sh
alert-publisher --enabled --journal-backend r2 --initialize-journal \
  --r2-config-file /config/r2-journal.json \
  --r2-credential-file /credentials/journal.json
```

Initialization uses `If-None-Match: *` and reads the committed object back,
checking its acknowledged revision, random identity and instance. It refuses an
existing object. An uncertain initialization returns failure, never a receipt
or automatic retry. A missing live journal is an error, even if a prior write
has an uncertain outcome: a negative read does not cancel an in-flight write.

Normal explicit activation uses the same storage files alongside the existing
App's projected private key and installation fact:

```sh
alert-publisher --enabled --journal-backend r2 \
  --r2-config-file /config/r2-journal.json \
  --r2-credential-file /credentials/journal.json \
  --key-file /credentials/app.pem --installation-id INSTALLATION_ID
```

Do not pass `--state-dir` in remote mode. `--drain` remains available. Disabled
commands read no input, credentials or network; invalid invocations and
observations stop before credential loading. Initialization cannot be combined
with draining or GitHub publishing facts.

Each invocation reads the complete bounded object and its ETag together and
owns a separate revision. Every journal barrier uses `PutObject` with
`If-Match`, advancing a generation and random nonce only after an acknowledged
new revision. Writes are paced at one per second per backend instance;
provider throttling, conflicts, cancellation, missing acknowledgements and
uncertain responses stop that invocation without another GitHub write or a
local fallback. The provider still enforces limits across independent processes.
Remote envelopes are bounded at 132 KiB around the unchanged 128 KiB journal.
Wrong instance, partial or ambiguous JSON and invalid revisions are refused.

The saved send intent still precedes every POST. Loss of its storage response
never authorizes sending; later invocations reconcile the persisted operation
marker. Receipt persistence also uses the conditional session. A UTC-day change
after persistence defers the unsent operation durably, so its next attempt
reserves the new day's budget. Expiry and cancellation are checked again after
the storage wait before publication.

### Recovery and activation limits

Conditional writes protect competing current writers. They **cannot detect a
privileged restore of an older valid object**. Generation and nonce do not
provide an independent rollback floor. This publisher does not automate a
restore or repair corrupt state. Keep all publishers disabled, establish that
prior in-flight writes have ended, and use a reviewed operator recovery
procedure that reconciles the authoritative bot-authored GitHub objects,
operation markers, receipts and current-day budget before replacing state.
Missing or duplicate markers and incomplete reads remain quarantined. Never
hand-edit the live object, initialize it again as a retry, or replay an old
snapshot as if it established fresh state.

This source is not live storage or route acceptance. Before activation, verify
the actual private bucket, retention and lifecycle rules, available least
privilege and independent credential/state custody through the declarative
Cloudflare owner. Keep the bootstrap credential out of the publisher; its
storage client issues only GetObject and conditional PutObject. Do not reuse a
backup credential or publish the journal via a custom domain. Demonstrate real
provider conflicts, uncertain writes and runner-crash recovery, then bind the
fixed heartbeat and cut over routes with rollback. Neither a local file copy
nor a runner cache/artifact is a transactional journal. The controlled inbox,
phone and same-thread acceptance in #2755 remains required.

The opt-in native storage test uses the actual R2 client and a dedicated proof
configuration. Its key must be `proofs/alert-journal-native-v1.json` and its
instance must be `alert-journal-native-proof`; the ordinary journal target is
refused. Use only the independently approved journal proof bucket and credential,
never a backup credential. With both private JSON files prepared, run once:

```sh
ALERT_R2_PROVIDER_PROOF=run-once \
ALERT_R2_PROOF_CONFIG_FILE=/private/proof/config.json \
ALERT_R2_PROOF_CREDENTIAL_FILE=/private/proof/credential.json \
go test -count=1 -v ./scripts/alert-publisher \
  -run '^TestR2NativeProviderConflictLostResponseAndProcessExit$'
```

It refuses an existing proof object, demonstrates actual conditional-write
conflicts, hides one real committed write's acknowledgement from its caller,
and reads a durable intent after an owned child exits without cleanup. It emits
only a closed verdict and retains the proof object. A failed or interrupted
attempt must be investigated; do not delete or replace it to retry. Ordinary
tests skip this proof. A skip or an offline fixture pass does not establish
provider acceptance, and this storage proof does not establish GitHub delivery
or controlled inbox and phone acceptance.

Sources: [R2 consistency and in-flight writes](https://developers.cloudflare.com/r2/reference/consistency/),
[conditional PutObject](https://developers.cloudflare.com/r2/api/s3/extensions/#conditional-operations-in-putobject),
and [AWS SDK retry policy](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-retries-timeouts.html).
