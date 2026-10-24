# Private mail: isolated preparation before activation

## Decision

Use Stalwart Community on the existing platform for the private mail capability
requested in #5030. The preparation slice is #5288. Version v0.16.27 was released
on 18 October 2026; its source is available under AGPL-3.0-only or the vendor's
commercial alternative. This deployment uses the Community functionality and
introduces no paid service or Enterprise dependency. The published image index
is pinned by digest as well as release tag.

Stalwart supplies SMTP, IMAP/JMAP, DKIM and sender authentication together. Its
supported declarative path uses a small datastore configuration and the official
CLI to reconcile the remaining settings through its management API. Keeping this
capability on the existing platform avoids a separate mail-only repository or an
unnecessary external processor of private message contents.

Provider discovery was performed before selection: the Directory had no
self-hosted-mail result. The server's exact release, licensing, source and
official deployment documentation were then inspected. An external relay remains
a separate decision if direct outbound delivery cannot be established.

## Preparation is disabled by default

The `k8s/bases/apps/private-mail` bundle is deliberately absent from both normal
application overlays. Its explicit inclusion is the coarse deployment toggle.
Rendering the bundle directly tests the enabled preparation state; rendering
both provider application layers tests that the default state remains absent.

Even when included, this revision starts only the internal recovery management
API. It has no public Service or route, mail listener or outbound network grant.
Only a setup workload in the same namespace can reach its management port. The
server receives no Kubernetes API token. Its single local datastore owner is
explicitly exempted from the serving-workload replica floor; this is not the
availability decision for public mail.

An External Secrets password generator creates the initial credential. A
non-overwriting PushSecret preserves it in OpenBao; the server consumes only the
credential read back from OpenBao. Losing the generator cache cannot replace the
durable administrator. Supplying the recovery administrator before first start
prevents Stalwart from generating a temporary password into its logs. No human
or agent reads the credential as part of this preparation.

The namespace and claim retain the platform's current prune protection. The
claim is not a backup or restore proof. No mailbox content exists in preparation.

## Activation and completion

The parent #5030 remains open until the service delivers and receives real mail.
Activation must supply and validate all of the following before opening public
listeners:

- Migration of the existing custom-domain mailbox from iCloud, as requested on
  24 October 2026. Inventory the domain's other addresses privately, preserve
  their receiving continuity during retirement, and preserve existing messages.
  Verify the destination, imported folders and rollback before changing the
  domain-wide MX records through the owning DNS repository.
- Usable public TLS, MX, SPF, DKIM and DMARC. The mail hostname can be a subdomain;
  the mailbox domain is the existing custom domain. Keep iCloud and its history
  intact until the migration is verified. Never delete shared iCloud history as
  a side effect of moving this mailbox.
- Verified maintainer-owned recipients and a server-enforced restriction for the
  agents' account, covering SMTP and any permitted JMAP submission path.
- An observed outbound path. Hetzner's default port restriction is a provider
  fact, not proof of the account's current state. Prefer direct delivery when
  available; a provider unblock or an external relay needs its own resolved step.
- Idempotent configuration through the official CLI, a permanent administrator,
  and removal of the preparation recovery variables before normal operation.
- Replies tied to an unguessable message token and the server's evaluated DMARC
  result for an approved sender. Sender-provided authentication headers are not
  evidence of authentication.
- An inbox test with passing SPF/DKIM/DMARC, forged-reply and open-relay refusal,
  a refused unauthorized recipient, an exercised backup and restore, and the
  maintainer-performed credential handoff to the host's Keychain.
- A visible Stalwart entry on Homepage at `https://platform.devantler.tech`,
  with a working link to the authenticated administration interface. Register
  it with the serving administration route and verify the tile and navigation
  in a real browser after activation. The recovery API stays internal.

The public daily GitHub ask channel stays the accepted channel. Deploying mail
does not enable spend stewardship or designate its financial decision channel.
Those remain separate maintainer-owned decisions.

## Prepared account configuration

The default-off command `go run ./scripts/private-mail-config` prepares a native
CLI plan from private mounted files. It neither connects to a server nor starts
one. Its required flags are `--input-dir` and `--output-file`, both absolute
paths. The output must be a new file in a private directory; it is created with
mode 0600 and must live in memory-backed storage when used with real inputs.
The command emits fixed verdicts, never the plan or its inputs.

The input directory contains `settings.json`, `administrator-password`,
`maintainer-password`, `agent-password` and `listener-inventory.json`. Settings
contain only `domain`, `hostname`, `maintainer`, `agent` and `administrator`;
account names are distinct local parts. Passwords are separate generated
40–128 character alphanumeric values, with no trailing newline. Unknown or
duplicate settings, invalid names, broad file permissions and an existing output
are refused. Exact production addresses stay in private operator configuration.

Obtain the listener inventory with the committed
`scripts/private-mail-config/inventory-request.json` through the authenticated
management API in the same exclusive recovery session. Its bounded query/get
checks totals, returned IDs, missing objects and every listener's bind, protocol,
TLS and proxy settings. Preparation permits an empty fresh store or an exact
subset of the four prepared profiles. Unknown listeners, old default listeners,
incomplete reads and enabled hooks, milters or webhooks require reconciliation;
they are never silently removed. After apply, require all four exact profiles.
An empty CLI query is not this completeness proof.

The same bounded inventory also covers every local domain and its aliases.
The reserved rejection domain must be absent from both, and external directory
resolution is refused. A reserved public DNS suffix alone cannot prove that a
mail server will refuse local delivery. The settings cannot register a reserved
`.invalid` domain as the mailbox domain.

Apply the private plan with the pinned official Stalwart CLI 1.0.13, using its
environment-based authentication from mounted credentials. Capture both output
streams privately, check every operation and read back the resulting policy.
Do not enable debug output, print plans or put passwords in command arguments.
The plan updates the agent account by the exact ID returned by its upsert,
preserving accounts, mail and certificates on replay. It replaces the complete
permission set and expression maps so stale grants or recipient branches cannot
survive. The normal server reads TLS files at `/etc/stalwart/tls/tls.crt` and
`/etc/stalwart/tls/tls.key`; its default certificate references that object.

The human account retains normal authenticated SMTP and IMAP over TLS. Personal
Inbox history has no digest expiry policy. The separate agent account has only
the reviewed authentication, send/receive and JMAP read permissions. Its exact
approved recipient is enforced before local recipient resolution; other local,
remote and subaddress recipients are refused. MX offers no authentication;
submission requires TLS and authentication before accepting MAIL and enforces
the sender identity. Post-RCPT scripts are disabled. The HTTP upload endpoint is
denied because its native path does not honor the JMAP mutation permission list.
This supports IMAP/SMTP mail apps; full human JMAP attachment upload is not part
of this configuration.

The native fixture `scripts/tests/test-private-mail-configuration.sh` starts its
own pinned server in explicit recovery, applies and reapplies the real plan,
exercises wider-policy and unexpected-listener negatives, and tests allowed
delivery, denied recipients/relay/writes/upload and human IMAP/TLS. It repeats
configuration after real fixture mail exists and reads that message again.
These are disposable `.test` accounts, not proof of public mail delivery.

The explicit `bootstrap` subcommand owns an exclusive recovery child and the
datastore-volume lock. It reads mounted inputs, queries the real inventory,
applies the plan through the pinned CLI, requires a successful receipt for every
operation and reads the resulting policy back. Permission, listener-bind and
expression maps require exact equality, including removal of stale entries.
All native output is kept out of logs. A cancellation or failure stops and
collects the owned child; a forced kill cannot produce success. A clean recovery
shutdown is required before exit zero can allow a separate serving container to
start. The command never starts that serving container itself.

The bootstrap image contains the exact upstream server and CLI plus the static
Go initializer. Its default entrypoint is bootstrap; it cannot silently fall
through to an unconfigured normal server. It expects private inputs under
`/etc/private-mail`, the datastore at `/var/lib/stalwart`, native configuration
and TLS at `/etc/stalwart`, and memory-backed `/tmp`. The dedicated publisher
builds without publishing for PRs. Protected-main publication adds SBOM,
provenance and a verified Cosign signature to the immutable image digest.
Production manifests must use that signed digest and keep public activation
separate. API liveness or closed mail ports alone are insufficient.

The native fixture also runs the actual bootstrap image, rejects unsafe stored
inventory, proves normal authentication after recovery stops, and repeats
bootstrap after receiving mail to verify history survives. A failed or
interrupted apply is not atomic and must keep serving stopped. This
configuration slice changes no running workload, public route or DNS record.
Client setup, private credential handoff, migration, public sender-authentication
proof and a production backup restore remain in #5030.

## Sources

- [Pinned release](https://github.com/stalwartlabs/stalwart/releases/tag/v0.16.27)
- [Pinned community license](https://github.com/stalwartlabs/stalwart/blob/v0.16.27/LICENSES/AGPL-3.0-only.txt)
- [Declarative deployments](https://stalw.art/docs/configuration/declarative-deployments/)
- [Kubernetes deployment and restricted security context](https://stalw.art/docs/cluster/orchestration/kubernetes/)
- [Hetzner outbound mail policy](https://docs.hetzner.com/cloud/servers/faq/#why-can-i-not-send-any-mails-from-my-server)
