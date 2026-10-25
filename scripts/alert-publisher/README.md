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
