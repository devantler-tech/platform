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
| Reports | DMARC, DKIM and SPF failure reports | off | They carry message content to whoever a sender's domain names. Aggregate DMARC and TLS reports stay on. |
| Retention | Trash and Junk expunge | 30 days | Pins the upstream default. |
| Throttles | Sender IP, sender-to-recipient, and submission per account | 5 per second, 25 per hour, 100 per hour | The server seeds the first two only into an empty collection, so all three are declared. |

## Drift check

`private-mail-bootstrap check --url http://<host>:8080` reads the serving
configuration back as the managed administrator and compares it with the
declared plan, without writing. It exits 0 when they match, 1 on drift (a
setting changed by hand, an unexpected listener or account, or a setting it
could not read) and 2 when it could not run. A scheduled Job runs it so a change
made in the web interface is reported before the next start reverts it.

## Before public SMTP and IMAP

The public SMTP and IMAP listeners take the real client address from the mail
load balancer's PROXY header only when the deployment names that load balancer
(`--mail-proxy-source`, which production sets). Without it every client shares
one address, so a single ban or throttle would block all of them. Still to be
declared on those listeners:

- Bans and the sender IP throttle on the SMTP and IMAP listeners.

Before MX moves, the spam filter also needs its rules. They download from an
unpinned upstream URL that egress blocks, and without rules every score is zero.
Stalwart 0.16.27 can import a copy pinned into the image instead, because
`spamFilterRulesUrl` accepts a `file://` path, and spam-filter v3.0.2 is the
last release in its rule format. That release also brings 17 DNS blocklists and
a check that requests shortened links found in received mail. Egress allows DNS,
so the blocklists would send sender addresses, the domains a message names and
hashed sender addresses to their operators. Egress denies the link requests, so
each such link would wait out a five-second timeout. 0.16.27 cannot import the
rules with either turned off, so the import waits for a decision: accept both,
or import a reviewed copy with the blocklists disabled and the link list left
out (#5387).

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
