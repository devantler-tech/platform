# Email secure defaults

The setup program (`scripts/private-mail-config`) declares the Email server's
security settings, applies them at every start and reads each one back before the
server serves. A setting changed in the web interface returns to its declared
value at the next start. The audit behind these values is recorded on #5387; it
was checked against the Stalwart v0.16.27 source.

## Declared

| Area | Setting | Value | Why |
| --- | --- | --- | --- |
| Inbound checks | DKIM and ARC verification | relaxed on `smtp-inbound`, off elsewhere | Upstream keys every verification on local port 25, and the inbound listener is 1025. The server ignores a quoted value and falls back to relaxed on every listener, so each verification value is a bare constant. |
| Inbound checks | SPF verification of the EHLO name and MAIL FROM; reverse-IP verification | relaxed on `smtp-inbound` when the deployment names its mail load balancer (`--mail-proxy-source`, which production sets); off otherwise | The listener then takes the client address from that load balancer's PROXY header, so these judge the sender. Without it the address could be the load balancer's own, so they stay off. Relaxed records each result in the Authentication-Results header and for the spam filter. The MAIL FROM result, or the EHLO result when MAIL FROM is empty, also feeds DMARC; reverse IP never does. Strict would refuse every sender whose domain publishes no SPF record and every host without matching reverse DNS. |
| Inbound checks | DMARC verification | strict on `smtp-inbound` when the deployment names its mail load balancer, relaxed there otherwise; off elsewhere | Strict applies the sender's published policy. A message that fails DMARC is refused when its domain publishes `p=reject`, and deferred when that result is a temporary error. A domain without a record, `p=none` or `p=quarantine` is accepted, and so is a message whose policy could not be read. Quarantine only adds to the spam score. A mailing list or forwarder that passes on mail from a `p=reject` domain and breaks its DKIM signature makes that mail fail DMARC, so it is refused: the refusal does not consider the ARC result. |
| Inbound checks | Non-FQDN HELO rejection; Received and Return-Path headers | on `smtp-inbound` only | The same port-25 keying left them off. |
| Inbound checks | Authentication-Results header | on `smtp-inbound` only | Outbound mail no longer carries this server's verdicts. |
| SMTP | VRFY and EXPN | off | Address enumeration. |
| Bans | Period for authentication, abuse, loiter and scan bans | one day | Upstream bans never expire. Web clients reach the server with their own address (#5448). |
| Logging | Bans and blocked addresses | logged | They name only an address. Sign-in and mail activity stay out of the console log (#5416). |
| Spam | Pyzor lookups | off | Egress allows only DNS and the relay, so they only time out, and they would also send message digests to a third party. |
| Spam | Classifier training samples | kept 30 days | Samples are private message data. |
| Spam | Filter rules | spam-filter v3.0.2 from the image, without the shortened-link list | The upstream default downloads the latest release, which egress blocks, so every score was zero. See [Spam filter rules](#spam-filter-rules). |
| Reports | DMARC, DKIM and SPF failure reports | off | They carry message content to whoever a sender's domain names. Aggregate DMARC and TLS reports stay on. |
| Retention | Trash and Junk expunge | 30 days | Pins the upstream default. |
| Throttles | Sender IP, sender-to-recipient, and submission per account | 5 per second, 25 per hour, 100 per hour | The server seeds the first two only into an empty collection, so all three are declared. |
| Submission | Sign-in required and offered over TLS, sender checked, submission throttle | on both `submission` (587, STARTTLS) and `submissions` (465, implicit TLS, served only with `--hostname`) | Each rule is built from one list of the submission listeners, whether or not 465 is served. A rule that named only one would let the other accept mail without sign-in. Without `--hostname` the setup removes a stored `submissions` listener, so the previous setup image can start on the store. |
| Server names | Default hostname; web host | `--hostname` (production: `mail.devantler.tech`), else the settings' `hostname`; `--web-hostname` (production: `stalwart.platform.devantler.tech`) | The default hostname is the SMTP greeting and EHLO name and the host mail apps are given for IMAP and SMTP. The serving process's `STALWART_PUBLIC_URL` names the same web host for OAuth and JMAP. |
| Advertised services | What autoconfig, autodiscover and the PACC document offer | IMAP and SMTP at the default hostname with implicit TLS only (993, 465); JMAP, CalDAV, CardDAV and WebDAV at the web host; no POP3 or ManageSieve | Upstream also advertises POP3 and ManageSieve, which this server does not serve. Upstream always offers 465 for SMTP, so without `--hostname` it is offered but not served, as before. Read back exactly, so a stored POP3 entry is drift. |
| HTTP | Upload endpoint | refused except at the maintainer's own upload URL | The endpoint ignores the JMAP permission list, and the access rule runs before sign-in, so it can only name the maintainer's account in the path. Another signed-in account can still store an unusable temporary upload there, counted against the maintainer's upload quota. On the first start of an empty store, before the account exists, uploads stay refused for everyone. |

## Drift check

`private-mail-bootstrap check --url http://<host>:8080` reads the serving
configuration back as the managed administrator and compares it with the
declared plan, without writing. It exits 0 when they match, 1 on drift (a
setting changed by hand, an unexpected listener or account, or a setting it
could not read) and 2 when it could not run. A scheduled Job runs it so a change
made in the web interface is reported before the next start reverts it. Give it
the same `--mail-proxy-source`, `--hostname` and `--web-hostname` as the setup:
without them it declares the settings' hostname and reports drift.

## Spam filter rules

The image fetches the stalwartlabs/spam-filter v3.0.2 release by its digest.
That release, built from tag commit `3d2eea7f`, is the last in the rule format
of Stalwart 0.16.27. The initializer derives the image's copy from it at build
time and refuses any other bytes or object counts. `spamFilterRulesUrl` names
that copy as a `file://` path, so the server never downloads rules. The
maintainer chose this set (#5387):

- **DNS blocklists on.** All 18 stay as upstream ships them, 17 of them
  enabled. The mail pod's DNS reaches any name, so they send sender IP addresses,
  the domains a message names and hashed sender email addresses to their operators.
- **Shortened-link check off.** For a link through one of the 1,076 hosts in
  the `url-redirectors` lookup, the filter requests the link to read where it
  leads. Egress denies those requests, so each link would wait five seconds.
  The copy leaves those keys out, and the check runs only for a listed host.
- **Remote lists unchanged.** The four lists the rules download over HTTPS
  (two phishing feeds, disposable and free-mail domains) are denied by egress,
  so they stay empty and the rules that use them never match.

The server imports the rules only at a normal start that finds none stored. A
later release therefore needs a new derived copy and a manual spam filter
maintenance task of type update rules; a new file alone changes nothing on a
store that already holds rules. That task only adds objects and replaces changed
ones. It never changes a stored tag, so a release's new tag scores need a manual
review and update; it keeps each HTTP list's enable flag, and it removes nothing
the new release dropped. For the same reason this copy turns the shortened-link
check off only on a store that holds no upstream `url-redirectors` keys. A store
that ever imported the upstream rules keeps those keys until their
`MemoryLookupKey` objects are destroyed. The production store never imported
them, because egress blocks the upstream download, so it has none. After a
restart, check that the lookup's key count is the derived 1,008 and that the
`url-redirectors` namespace has none. The 1.x candidate declares the upstream
default URL, so an update-rules task on a store migrated from 0.16 cannot read
the 0.16 copy again from the candidate image. It does not replace the rules such
a store already holds: the server imports only into an empty store. Adopting
that runtime therefore also pins spam-filter v3.0.4 and runs the update-rules
task, with the same review of stored tags and of any `url-redirectors` keys.

## Before public SMTP and IMAP

The public SMTP and IMAP listeners take the real client address from the mail
load balancer's PROXY header only when the deployment names that load balancer
(`--mail-proxy-source`, which production sets). Without it every client shares
one address, so a single ban or throttle would block all of them. Still to be
declared on those listeners:

- Bans and the sender IP throttle on the SMTP and IMAP listeners.

## Not declared

- **ASN lookups.** Disabled is the default value, so declaring it stores nothing,
  and at first normal start the server seeds a download source that egress then
  blocks. The drift check found this on the serving fixture.

- **Password policy.** The upstream minimum is 8 characters at strength three.
  Generated passwords are 40 or more random characters, so the policy binds only
  a reset made by hand. On a fresh store the server installs its native role
  defaults at first start only while the Authentication settings have no stored
  row. Storing a stricter policy during setup creates that row and leaves the
  administrator without permissions, so the setup stores only default values there.

- **Web sign-in provider.** Anonymous client registration is on and registered
  clients are not required. Declaring the provider during setup on a fresh store
  would stop the server generating its signing keys at first start, and requiring
  registration needs a registered client for the web interface first.
- **Two-factor sign-in.** Stalwart 0.16.27 has no switch to require it, and app
  passwords are created only by the account itself. A TOTP seed for the
  administrator could be declared from OpenBao once the maintainer enrols it.
- **TLS 1.3 only.** TLS 1.2 stays for mail servers that lack 1.3.
- **HSTS.** The gateway route already sends a stronger header than the server's
  fixed one.
