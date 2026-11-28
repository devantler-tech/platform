# Company email: isolated preparation before activation

## Decision

Use Stalwart Community on the existing platform for the company email capability
requested in #5030. The preparation slice is #5288. Version v0.16.27 was released
on 18 October 2026; its source is available under AGPL-3.0-only or the vendor's
commercial alternative. This deployment uses the Community functionality and
introduces no paid service or Enterprise dependency. The published image index
is pinned by digest as well as release tag.

Stalwart supplies SMTP, IMAP/JMAP, DKIM and sender authentication together. Its
supported declarative path uses a small datastore configuration and the official
CLI to reconcile the remaining settings through its management API. Keeping this
capability on the existing platform avoids a separate mail-only repository or an
unnecessary external processor of confidential company correspondence.

Provider discovery was performed before selection: the Directory had no
self-hosted-mail result. The server's exact release, licensing, source and
official deployment documentation were then inspected. An external relay remains
a separate decision if direct outbound delivery cannot be established.

## Shared preparation resources

The `k8s/bases/apps/private-mail` bundle supplies the shared storage and
initialization resources. The shared and local application layers omit mail;
production includes this base through the administration bundle below. Rendering
the base directly tests preparation, while the provider renders verify that only
production enables normal administration.

The preparation base alone starts only the internal recovery management
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
claim is not a backup or restore proof. Preparation neither migrates nor receives
messages, and must not be used as evidence that an existing volume is empty.

## Administration opt-in

The separate `k8s/bases/apps/private-mail-administration` bundle inherits the
preparation resources. Production includes it with encrypted settings; the
shared and local application layers keep mail disabled.
Its initializer must finish and stop recovery before Kubernetes starts the
normal Stalwart process. Both use the same immutable published bootstrap image.
Only the initializer receives the private settings and account passwords;
normal serving receives the datastore and certificate files. Each process has
its own memory-backed temporary directory, and neither receives Kubernetes API
credentials.

Maintainer and agent passwords follow the existing administrator's durable
generation pattern: create a cache once, push only if absent, then consume the
value read back from OpenBao. Deleting a generator cache cannot rotate an
existing account password. Missing settings, credentials or certificate files
block startup. Production settings remain encrypted outside the shared bundle.
The credential synchronizer receives exact-key seed and read permissions for
all three durable credentials, with no mail-path wildcard, list or delete grant.
Admission and node image verification route only the exact bootstrap image
repository to its dedicated main-only publisher; other images keep their own
signer requirements.

The administration Service and HTTPS route select only normal management port
8080. The network policy admits only gateway ingress there, explicitly denies
recovery and prepared mail ports from every source, and permits egress only to
DNS and to the Amazon SES relay on port 587 ([the relay decision](email-outbound-relay.md));
the preparation base alone keeps its all-egress denial. The route registers an Email Homepage entry in the Email section at its
authenticated administration path. Inclusion prepares browser evaluation; it does not prove
the Homepage entry, authentication or any production behavior until observed.
The route serves both `mail.${domain}` and `stalwart.${domain}`, DNS-only, and the Homepage entry
links to `stalwart.${domain}/admin`: Stalwart's web side moves there so that the Bulwark webmail
can take `mail.${domain}` ([#6662](https://github.com/devantler-tech/platform/issues/6662)). The
route adds CORS response headers that allow only Bulwark's origin, `https://mail.${domain}`,
without credentials; Stalwart's own permissive CORS stays off.
Public mail, DNS migration, credential custody, history import and restoration
remain separate activation requirements below.

## Email credential custody

The canonical operator-facing name is **Email**, for company email
infrastructure. Its credential destination is the `secret/` mount under
`infrastructure/email/admin`, `infrastructure/email/maintainer` and
`infrastructure/email/agent`. Retained namespace, datastore and image identifiers
keep their existing `private-mail` names during this migration.

The main-only **Verify Email Credential Custody** workflow runs the canonical
`scripts/prepare-email-credentials.sh` in fixed `plan` mode. Dispatch it on `main`
with confirmation `verify-email-credential-custody`. It reads the current durable records under `apps/private-mail/`,
compares any existing destination, and reports pending copies without changing
credentials or consumer paths. It reuses the existing recovery credential over
a loopback connection, with private memory-backed scratch space; credentials and API
responses are never logged. Missing or unreadable data is distinguished from a
valid plan, and a different destination fails closed. The workflow rejects an
unseeded or incomplete result and exposes only validated counters for all three
records. It has no copy input and cannot activate migration. No credential-handling
program is embedded in a Kubernetes ConfigMap or added to the configuration Job.

After snapshot recovery has been verified, a separate reviewed revision may
activate `copy` mode. Creation uses KV v2 compare-and-set with version zero and
copies the complete current data object. It reads back and compares the result,
rechecks the source, and verifies a same-value creation race rather than
overwriting it. Numeric extra fields are refused to avoid JSON precision loss.
An uncertain result remains retryable. Existing source records
and their history remain intact. Only a native production `VERIFIED` result for
all three records permits a later consumer-path switch. A plan or an `UNSEEDED`
result is not proof of migration.

Consumer changes, exact-path authorization and readback must land after the copy
is verified. Readers and non-overwriting seed writers use the same three
`infrastructure/email/` records. Their Kubernetes Secret names and generator
caches remain unchanged. The synchronizer has no write grant for the retained
legacy records, and no list, delete or wildcard Email grant.

The independent recovery workflow compares the canonical records using its fixed
`collect-email` and `verify-email` modes. Its legacy modes remain available to
the guarded one-time copy workflow and retained-history recovery; the canonical
mode never falls back to a legacy record when a canonical record is absent.
Retire the old active keys only after the consumers and recovery
path are proven; preserve recoverable versions rather than destroying metadata.
The preparation mechanism can then be removed. Rebuilding from an empty store
is distinct from moving existing credentials and must not replace restored
credentials with a generator cache.

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
- An observed outbound path. The maintainer chose a hosted relay, Amazon SES,
  over direct delivery; [the relay decision](email-outbound-relay.md) records
  its steps.
- Idempotent configuration through the official CLI, a permanent administrator,
  and removal of the preparation recovery variables before normal operation.
- Replies tied to an unguessable message token and the server's evaluated DMARC
  result for an approved sender. Sender-provided authentication headers are not
  evidence of authentication.
- An inbox test with passing SPF/DKIM/DMARC, forged-reply and open-relay refusal,
  a refused unauthorized recipient, an exercised backup and restore, and the
  maintainer-performed credential handoff to the host's Keychain.
- A visible Email entry in the Email section on Homepage at
  `https://platform.devantler.tech`,
  with a working link to the authenticated administration interface. Register
  it with the serving administration route and verify the tile and navigation
  in a real browser after activation. The recovery API stays internal.

The public daily GitHub ask channel stays the accepted channel. Deploying mail
does not enable spend stewardship or designate its financial decision channel.
Those remain separate maintainer-owned decisions.

The owned bootstrap process uses recovery management port 18080, separate from
normal administration on 8080. The pinned server supports a recovery port
override but [binds it to every interface](https://github.com/stalwartlabs/stalwart/blob/v0.16.27/crates/common/src/config/server/listener.rs#L59-L84),
so the administration overlay excludes 18080 from every Service and ingress
grant, including during initialization and retries. Readiness alone is not that
boundary. Bootstrap sets the override explicitly rather than inheriting it, and
stops and collects its recovery child before permitting normal serving.
Production exposes only normal administration after this clean recovery exit.
Public mail transport and migration remain separate activation requirements.

### Amendment, 15 November 2026: listeners before migration

The maintainer approved the inbound plan on #5481, including the mail server's
own Hetzner load balancer (LB11) as its one new recurring cost. That plan opens
the SMTP, submission and IMAPS listeners before the mailbox history moves from
iCloud: the history copy, mail-app setup and the external-network checks all
need a reachable destination first.

This amends the activation list above in one respect only. The listeners may
open before the migration. The domain's MX records still move only after the
destination, the imported history and the rollback are verified, and only with
the maintainer's approval at that time. Until MX moves, no external sender
delivers here. Open relaying, unauthenticated submission and a sender that does
not match the signed-in account stay refused from the first opening, and the
agents' account stays limited to the maintainer.

The listeners take the client address from PROXY protocol sent by the mail
load balancer itself. The shared Gateway cannot carry it: its TCP passthrough
opens a new connection from Envoy. Each mail listener trusts PROXY headers from
the load balancer's one pinned private address only
([PROXY identity](../operations/email-proxy-identity.md#mail-listeners-behind-their-own-load-balancer)).

### Amendment, 25 November 2026: the server's public identity

The mail server's public name is `mail.devantler.tech`: the host mail apps use
for IMAP and submission, and the host the domain's MX record will name. Until
now the server's default hostname was the web host of its administration,
`mail.platform.devantler.tech`. Its SMTP greeting, EHLO name and Message-ID
domain named that web host, and its autoconfig sent mail apps there, to ports
it does not serve (#6662). Two announced restarts change the following.

- The deployment names the server with the setup's `--hostname` flag. The flag
  takes precedence over the `hostname` in the encrypted settings, which applies
  only without it, so removing the flag is the rollback. The settings also hold
  the account names; the public name is a reviewed, non-secret flag instead.
  The same flag serves implicit-TLS submission (below): the name and the port
  its autoconfig offers ship together.
- The serving process's `STALWART_PUBLIC_URL` is
  `https://stalwart.platform.devantler.tech`, the web route's host. Stalwart
  0.16.27 builds its OAuth issuer and metadata and the URLs in JMAP sessions
  from it; without it, from the default hostname, which now serves no HTTPS.
  The setup's `--web-hostname` gives autoconfig and autodiscover the same host
  for JMAP, CalDAV, CardDAV and WebDAV.
- With `--hostname`, submission is also served with implicit TLS on 465
  (RFC 8314), from the mail load balancer with PROXY protocol, like 587. Every
  rule that requires sign-in, offers it, checks the sender or throttles
  submission is built from one list of both listeners, whether or not 465 is
  served, and a test fails if any names only one. Without the flag the setup
  removes a stored `submissions` listener by its unique name. Leaving it would
  keep a listener the plan no longer declares, so the readback could not be
  exact, and the previous setup image, which refuses any listener it does not
  know, could never start on that store again. The removal is idempotent.
- The services the server advertises are declared: IMAP and SMTP at the mail
  host with implicit TLS only, so autoconfig offers 993 and 465, and no POP3 or
  ManageSieve, which the server never served. Submission on 587 stays served
  for clients set up by hand. Upstream always lists 465 for SMTP, so without
  `--hostname` autoconfig offers it unserved, as it did before.
- The upload endpoint is open at the maintainer's own upload URL only, on the
  maintainer's decision of 25 November 2026, so webmail can attach files, save
  filters and set a vacation reply. Stalwart's HTTP access rule runs before
  sign-in and sees only the request, so it names the maintainer's account in
  the path. The upload itself does not check that the signed-in account owns
  that account: another signed-in account can store a temporary upload there,
  counted against the maintainer's upload quota, but cannot read or use it. The
  agent's and the administrator's own upload URLs stay refused. Until the
  maintainer's account exists, on the first start of an empty store, uploads
  stay refused for every account.

A failed merge-group deployment is healed by republishing `main`, so every
step must be safe to heal back from. The change lands in three units:

1. **Code.** The setup program, its tests and these documents. No manifest
   changes and nothing restarts. The published image is signed from `main`.
2. **Adopt the image** (first announced restart). Only the image digest
   changes; no flag is passed. The new setup writes nothing the previous setup
   image refuses: it stores no `submissions` listener, and the previous setup
   rewrites the services, upload rule and submission rules it declares (its
   readback compares only what it declares). It does change the upload rule
   and stops advertising POP3. A heal or a rollback restores the previous
   digest, and its setup starts on that store.
3. **Enable** (second announced restart). `--hostname=mail.devantler.tech`,
   `--web-hostname`, `STALWART_PUBLIC_URL`, port 465 on the mail load balancer
   and its network admission. The web route's `stalwart.platform.devantler.tech`
   host must be live first, because the public URL names it. The voucher
   collector moves its JMAP URL and egress to that host in the same unit: it
   refuses a session whose URLs name another host. A heal or a
   rollback returns to unit 2's manifests: the same image without the flags,
   whose setup removes the `submissions` listener and returns the server to the
   settings' name, from which it again builds its public web address.

To roll back further than unit 2, roll back unit 3 first, so that the
`submissions` listener is gone before the previous image starts.

## Prepared account configuration

The default-off command `go run ./scripts/private-mail-config` prepares a native
CLI plan from private mounted files. It neither connects to a server nor starts
one. Its required flags are `--input-dir` and `--output-file`, both absolute
paths. The output must be a new file in a private directory; it is created with
mode 0600 and must live in memory-backed storage when used with real inputs.
The command emits fixed verdicts, never the plan or its inputs.

The input directory contains `settings.json`, `administrator-password`,
`maintainer-password`, `agent-password`, `relay-username`, `relay-password` and
`listener-inventory.json`. Settings
contain only `domain`, `hostname`, `maintainer`, `agent` and `administrator`;
account names are distinct local parts. Passwords are separate generated
40–128 character alphanumeric values, with no trailing newline. The relay login
is the send-only SES SMTP username, an access key ID, and its SES SMTP password,
read from OpenBao `infrastructure/email/ses-smtp`. Unknown or
duplicate settings, invalid names, broad file permissions and an existing output
are refused. Exact production addresses stay in private operator configuration.
The server's public names are not private, so the deployment gives them as
flags rather than in the encrypted settings: `--hostname` takes precedence over
the settings' `hostname`, which applies only without it, and `--web-hostname`
names the web host (see the
[25 November amendment](#amendment-25-november-2026-the-servers-public-identity)).

Obtain the listener inventory with the committed
`scripts/private-mail-config/inventory-request.json` through the authenticated
management API in the same exclusive recovery session. Its bounded query/get
checks totals, returned IDs, missing objects and every listener's bind, protocol,
TLS and proxy settings. Preparation permits an empty fresh store or an exact
subset of the six prepared profiles. Unknown listeners, old default listeners,
incomplete reads and enabled hooks, milters or webhooks require reconciliation;
they are never silently removed. After apply, require exactly the declared
profiles: all six when `--hostname` serves implicit-TLS submission, otherwise
the other five, the `submissions` listener being the one declared removal.
An empty CLI query is not this completeness proof.

The declared mailbox receives the pinned runtime's normal-user permissions as
an explicit replacement profile. Fresh recovery has no generated Role objects,
so initialization cannot depend on finding a role named User. Freeze the
individual permission names from the reviewed runtime rather than accepting
future permissions by a prefix. The separate administrator and the service
account's restricted replacement permissions remain intact. Readback requires
exact permission maps, including absence of undeclared entries.

External sign-in remains disabled during this local baseline. A later identity
activation must explicitly clear implicit user roles and verify their absence
after restart before enabling the external directory. On an empty datastore,
an update containing only default-valued authentication fields does not prevent
the first normal startup from installing native role defaults; it is not an
identity-enrollment restriction. Mailbox permission preparation therefore makes
no claim that external enrollment is ready or enabled.

The same bounded inventory also covers every local domain and its aliases.
The reserved rejection domain must be absent from both, and external directory
resolution is refused. A reserved public DNS suffix alone cannot prove that a
mail server will refuse local delivery. The settings cannot register a reserved
`.invalid` domain as the mailbox domain.

The inventory also covers every HTTP application. It accepts no application or
the pinned server's single default Stalwart Web Interface, with exactly the
`/admin` and `/account` prefixes. A missing or partial application census,
another application, an unknown resource, an external OAuth client or a custom
unpack directory fences initialization. The plan preserves the interface's
identity and replaces its route map with the two approved prefixes.
Known unversioned local resources and earlier digest-qualified resources are
accepted only as input to reconciliation to the active image's verified pin.

Apply the private plan with the official Stalwart CLI pinned for the selected
server profile. The deployed 0.16.27 profile uses CLI 1.0.13. The 1.0.2 and
1.0.3 profiles require CLI 1.0.14. Authenticate through the environment from
mounted credentials. Capture both output streams privately, check every
operation and read back the resulting policy.
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
submission, with STARTTLS on 587 and implicit TLS on 465, requires TLS and
authentication before accepting MAIL and enforces the sender identity. Every
rule that requires sign-in or checks the sender is built from one list of the
submission listeners, so 465 cannot accept mail that 587 refuses. Post-RCPT
scripts are disabled. The HTTP upload endpoint does not honor the JMAP mutation
permission list, so it is refused except at the maintainer's own upload URL
(see the [25 November amendment](#amendment-25-november-2026-the-servers-public-identity)).

The native fixture `scripts/tests/test-private-mail-configuration.sh` starts its
own pinned server in explicit recovery, applies and reapplies the real plan,
exercises wider-policy and unexpected-listener negatives, and tests allowed
delivery, denied recipients/relay/writes, the upload rule, submission on both
ports and human IMAP/TLS. It repeats configuration after real fixture mail
exists and reads that message again. Its last bootstrap takes production's
flags and reads back the server name, the advertised services, autoconfig,
autodiscover and the SMTP greeting. These are disposable `.test` accounts, not
proof of public mail delivery.

The explicit `bootstrap` subcommand owns an exclusive recovery child and the
datastore-volume lock. It reads mounted inputs, queries the real inventory,
applies the plan through the pinned CLI, requires a successful receipt for every
operation and reads the resulting policy back. Permission, listener-bind and
expression maps require exact equality, including removal of stale entries.
All native output is kept out of logs. A cancellation or failure stops and
collects the owned child; a forced kill cannot produce success. A clean recovery
shutdown is required before exit zero can allow a separate serving container to
start. The command never starts that serving container itself.

The deployed 0.16.27 bootstrap image contains the exact upstream server and CLI,
the static Go initializer, and the pinned Web Interface v1.0.11 bundle. Both
image build paths stage the bundle and its Community AGPL license after verifying
their committed SHA-256 checksums. The image includes the upstream source URL.
Bootstrap verifies the bundle again before starting recovery, refusing missing,
altered, oversized or symbolic-link assets. The application uses the image's
local file rather than downloading executable browser assets at runtime.
The filename and configured resource URL include the bundle's digest. Changing
the pin therefore changes the native resource URL and invalidates Stalwart's
cached ZIP while preserving the Application identity; verifying an image file
at a permanent URL alone would still permit a previously cached interface.

The compiler accepts an explicit `runtimeVersion` of `0.16.27`, `1.0.2`, or
`1.0.3`. Omitting it selects the deployed 0.16.27 schema. Unknown, empty, null
or duplicate selectors are refused. Both 1.0 profiles use native property
names, audited normal-user permissions, and Web Interface 2.0.4. Their inventory
and alias checks reject foreign or mixed schema fields. Before opening the
datastore, each 1.0 profile requires its exact server version and CLI 1.0.14.
To stage the verified 1.0.3 asset, run this from the repository root with
`build_dir` set to an existing absolute build directory.

```bash
bash scripts/private-mail-config/stage-webui.sh "$build_dir" 1.0.3
```

The 1.0.3 profile supports the full OAuth scope needed for recovery administrator
sign-in through a registered Web Interface client. The default container build
uses the deployed baseline. The publisher builds the 0.16.27 and 1.0.3 profiles
from their pinned server, CLI and Web Interface inputs. It checks the versions
inside the built 1.0.3 image without network access. Protected main publishes
the candidate to a separate image repository so the 0.16 recovery reader can
continue to require the legacy image repository. Selecting the candidate for
serving requires a separate deployment.

Selecting a schema does not migrate a datastore. Stalwart 1.0 refuses stores
created before 1.0. A coordinated upgrade requires the
[upstream migration procedure](https://stalw.art/docs/migration/upgrade-0-16/):
an empty target, a retained source and its external dependencies, stopped
writers for final synchronization, full verification with no error or mismatch
reports, and a tested rollback using the forward migration state. The migration,
image change, storage activation and identity cutover have separate deployment
and recovery gates.

Its default entrypoint is bootstrap; it cannot silently fall
through to an unconfigured normal server. It expects private inputs under
`/etc/private-mail`, the datastore at `/var/lib/stalwart`, native configuration
and TLS at `/etc/stalwart`, and memory-backed `/tmp`. The dedicated publisher
builds without publishing for PRs. Protected-main publication adds SBOM,
provenance and a verified Cosign signature to the immutable image digest.
Production manifests must use that signed digest and keep public activation
separate. API liveness or closed mail ports alone are insufficient.

The native fixture also runs the actual bootstrap image, rejects unsafe stored
inventory, and serves both interfaces from that same image on an internal
network without external egress. It compares every script and stylesheet
referenced by each entry page with the pinned bundle, proves authenticated
management access and refuses the same request without authentication. It
primes an old cached interface, reapplies bootstrap with the real image, and
requires the pinned pages and assets to replace that cache without changing
the Application identity. It
proves normal authentication after recovery stops, and repeats
bootstrap after receiving mail to verify history survives. A failed or
interrupted apply is not atomic and must keep serving stopped. This
configuration slice changes no running workload, public route or DNS record.
Client setup, private credential handoff, migration, public sender-authentication
proof and a production backup restore remain in #5030.

### Receiving continuity

Private settings may optionally declare `maintainerAliases`, a list of up to
16 distinct local address names on the configured domain. They become receiving
aliases of the maintainer mailbox, without separate credentials or broader
agent-sending permission. Omission leaves existing aliases unmanaged. An
explicit empty list cannot erase a nonempty native alias set.

The inventory includes complete bounded Account and MailingList query/get
responses: both primary addresses and disabled aliases reserve ownership.
Configured aliases require an empty or exactly approved existing maintainer
alias set; any competing owner, unexpected alias or incomplete census refuses
the plan. Bootstrap repeats this check on fresh data from its exclusively owned
recovery child immediately before apply. A saved standalone plan and inventory
are snapshots, not an atomic reconcile: the pinned CLI has no revision
precondition, so they must not authorize a later retry after live drift.

Readback compares the entire alias collection, resolving domain references and
native defaults without depending on numeric entry order. The native fixture
exercises actual delivery to both aliases, unknown-address refusal, competing
User/Group/MailingList ownership, stale snapshots, and omission/replay preserving
the retained account and delivered mail. Deployment and DNS remain separate.

## Deployment sequence

Deliver the dedicated mail publisher's admission and node verification rules,
together with exact-key credential seed/read permissions, in a prerequisite
revision while production mail administration remains disabled. Verify the
normal protected deployment and the exact declared rules on every node before a
separate revision enables administration.

That prerequisite also owns the namespace and claim through the storage-only
`k8s/bases/apps/private-mail-storage` bundle. It starts no process and exposes no
service. Preparation and administration inherit those same persistent identities.
When activation replaces the production storage-only reference, remove that direct
reference so the inherited resources are declared only once. A failed activation
can then return to a reviewed main that still owns the retained storage, keeping
it managed without deleting data or weakening the orphan-ownership check.

The storage-only claim carries a `storage-stage: prepared` marker. Its explicit
Flux health decision accepts Pending only for this exact claim on the default
`longhorn-wffc` class before provisioning has selected a node or volume. Lost or
terminating claims do not pass. The serving bundle removes the marker, so its
claim must be Bound; all other claims retain their binding requirement. Health
waiting remains enabled throughout, without changing the retained claim's spec.

The production pipeline reconciles applications before its full node configuration
update. Combining the prerequisite with an application that already needs the new
node rule makes that rollout wait on itself. Separate reviewed revisions preserve
the existing deployment sequence and signer isolation without a manual node
configuration change. Public mail activation and migration retain their separate
acceptance requirements.

After the protected rollout, the dispatch-only **Verify Email Storage
Recovery** workflow checks the original failed deployment's ownership window.
Run it from `main` with confirmation `verify-private-mail-storage-recovery`.
It uses the existing production reader while holding the deployment lock, makes
no cluster changes, and prints only a fixed verdict. Inventory, errors and
temporary credential files are not published. A missing read or incomplete scan
is unverified, never a pass. This verifies ownership recovery, not mail backup
or restore acceptance.

## Sources

- [Pinned release](https://github.com/stalwartlabs/stalwart/releases/tag/v0.16.27)
- [Pinned community license](https://github.com/stalwartlabs/stalwart/blob/v0.16.27/LICENSES/AGPL-3.0-only.txt)
- [Pinned Web Interface release](https://github.com/stalwartlabs/webui/releases/tag/v1.0.11)
- [Web Interface Community license](https://github.com/stalwartlabs/webui/blob/v1.0.11/LICENSES/AGPL-3.0-only.txt)
- [Declarative deployments](https://stalw.art/docs/configuration/declarative-deployments/)
- [Kubernetes deployment and restricted security context](https://stalw.art/docs/cluster/orchestration/kubernetes/)
- [Hetzner outbound mail policy](https://docs.hetzner.com/cloud/servers/faq/#why-can-i-not-send-any-mails-from-my-server)
