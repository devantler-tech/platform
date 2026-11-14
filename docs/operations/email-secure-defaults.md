# Email secure defaults

The setup program (`scripts/private-mail-config`) declares the Email server's
security settings, applies them at every start and reads each one back before the
server serves. A setting changed in the web interface returns to its declared
value at the next start. The audit behind these values is recorded on #5387; it
was checked against the Stalwart v0.16.27 source.

## Declared

| Area | Setting | Value | Why |
|---|---|---|---|
| Inbound checks | DKIM, DMARC and ARC verification | relaxed on `smtp-inbound`, off elsewhere | Upstream keys these on local port 25, and the inbound listener is 1025. The server ignores a quoted value and falls back to relaxed on every listener, so the values are bare constants. |
| Inbound checks | Non-FQDN HELO rejection; Received and Return-Path headers | on `smtp-inbound` only | The same port-25 keying left them off. |
| Inbound checks | Authentication-Results header | on `smtp-inbound` only | Outbound mail no longer carries this server's verdicts. |
| SMTP | VRFY and EXPN | off | Address enumeration. |
| Passwords | Minimum length and strength | 20 characters, strength four | Generated passwords are 40 or more random characters; this binds later resets. |
| Bans | Period for authentication, abuse, loiter and scan bans | one day | Upstream bans never expire. Web clients reach the server with their own address (#5448). |
| Logging | Bans and blocked addresses | logged | They name only an address. Sign-in and mail activity stay out of the console log (#5416). |
| Spam | Pyzor and ASN lookups | off | Egress allows only DNS and the relay, so they only time out. Pyzor would also send message digests to a third party. |
| Spam | Classifier training samples | kept 30 days | Samples are private message data. |
| Reports | DMARC, DKIM and SPF failure reports | off | They carry message content to whoever a sender's domain names. Aggregate DMARC and TLS reports stay on. |
| Retention | Trash and Junk expunge | 30 days | Pins the upstream default. |
| Throttles | Sender IP, sender-to-recipient, and submission per account | 5 per second, 25 per hour, 100 per hour | The server seeds the first two only into an empty collection, so all three are declared. |

## Before public SMTP and IMAP

These need the real client address on the public SMTP and IMAP listeners first.
Without it every client shares one address, so a single ban or throttle blocks
all of them, and SPF and reverse-IP checks judge the load balancer.

- SPF and reverse-IP verification on `smtp-inbound`, then strict DMARC.
- Bans and the sender IP throttle on the SMTP and IMAP listeners.

Before MX moves, the spam filter also needs its rules. They download from an
unpinned upstream URL that egress blocks, and without rules every score is zero.
Vendoring a pinned rules file restores scoring and the sender-domain blocklist.

## Not declared

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
