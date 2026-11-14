# Email outbound relay

**Status:** Accepted. The maintainer chose a hosted relay, Amazon SES in
`eu-central-1`, on 2026-11-13.

## Context

Company Email first planned direct delivery from the cluster (#5481). That needs
a stable sending address with matching reverse DNS, routed only for the Email
server. On 2026-11-13 the maintainer switched to a hosted relay: *"Let's switch
to a relay, but do a quick research to find the cheapest option that support my
business. We should be able to change to a more advancced and expensive option
later given it is just a relay, but no need to overcomplicatd things from the
start"*. No dedicated address is bought. Inbound mail, migration and recovery
are unchanged.

## Decision

- **Amazon SES** on the existing AWS account, pay-per-use, in `eu-central-1`.
  At about $0.10 per 1,000 messages it costs pennies at this volume, and its
  terms allow ordinary mailbox mail from our own domain.
- **The mail domain is an SES identity** with 2048-bit Easy DKIM, plus a custom
  MAIL FROM domain on a subdomain, so both SPF and DKIM align for DMARC. SES
  signs every relayed message. It rewrites Message-ID and Date, which can break
  Stalwart's own signature, but SES's aligned signature still passes DMARC.
- **Stalwart relays all outbound mail** to the SES SMTP endpoint on port 587,
  with STARTTLS and SMTP authentication.
- **One send-only credential.** An IAM user may only `ses:SendRawEmail` on that
  identity, with the sender limited to the mail domain. Crossplane creates its
  access key and publishes the key ID, which is the SMTP username, and the
  derived SES SMTP password. That password is valid only for the provider's
  own region, which is why SES runs in `eu-central-1`. A platform-applied
  PushSecret copies the username and password to one exact OpenBao key, and the
  Email server reads both from there. Nobody reads the values.
- **Ownership.** The identity and MAIL FROM domain grant no permission, so the
  `aws` tenant owns them. The SMTP user, its policy and its key stay
  platform-owned, like the EKS CI role, so the public tenant cannot widen them.
- **DNS.** The platform's external-dns publishes the three DKIM CNAMEs and
  the MAIL FROM domain's MX and SPF records as DNS-only `DNSEndpoint`
  resources. They are new names, so each keeps one owner; external-dns also
  manages MX and TXT records for this. SPF is checked on the MAIL FROM domain,
  so the mailbox domain's own SPF record, which still lists the existing
  provider, is unchanged. The existing DMARC record keeps its current owner.
- **Production access** is requested through a reviewed main-only workflow.
  Until SES grants it, its sandbox delivers only to verified recipients.
- **Swappable.** Changing relay means changing Stalwart's relay route and its
  credential; nothing else depends on SES.

## Order

1. Install the SES provider and activate the kinds (#6184).
2. Declare the identity and MAIL FROM domain (devantler-tech/aws#78).
3. Create the send-only SMTP identity and store its credential in OpenBao.
4. Publish the DNS records through external-dns once the identity's DKIM
   tokens exist.
5. Request production access.
6. Add Stalwart's relay route. Its network policy now denies all egress, and a
   deny overrides any allow, so this step replaces the deny with egress to DNS
   and to the SES SMTP endpoint on port 587 only. That ends the server's
   preparation-only network contract. It takes two revisions, because
   production runs only the signed bootstrap image published from `main`:
   1. The bootstrap configures the route, from two private inputs, the SMTP
      username and password. Every remote recipient goes to one relay route
      with STARTTLS required and the certificate checked. The route and the
      TLS strategy are named explicitly: the server falls back to direct MX
      delivery for an unknown route name, and to optional TLS for an unknown
      strategy. The password is stored as a value rather than a file path,
      because the server silently drops authentication when a secret file is
      missing or empty. Missing or malformed relay inputs stop the bootstrap,
      so serving stays stopped.
   2. Production adopts that image, supplies both inputs from the OpenBao key,
      and replaces the egress deny.
7. Send real mail to external mailboxes. Check that SPF, DKIM and DMARC pass,
   and that replies thread against the sender's Sent copy despite SES's
   Message-ID rewrite. If threading fails, report it before switching relay.

## Alternatives

- **Direct delivery:** needs a dedicated address, reverse DNS and egress
  routing for one server. Superseded by the maintainer on 2026-11-13.
- **SMTP2GO Free:** 1,000 messages a month, hosted in the EU, and its terms
  allow this use. It is the fallback if SES fails the test in step 7.
- **Scaleway TEM, Cloudflare Email Service:** transactional-only terms.
- **SES in `eu-north-1`:** the provider would publish an SMTP password for the
  wrong region, so it would need a separate derivation step.

## Consequences

The account starts in the SES sandbox, so real delivery waits on production
access. A lost or revoked access key stops outbound mail until Crossplane
creates a new one, and the push identity stores it again. Relayed mail and its
metadata are processed by SES in the EU.

## Sources

- [SES pricing](https://aws.amazon.com/ses/pricing/)
- [Requesting production access](https://docs.aws.amazon.com/ses/latest/dg/request-production-access.html)
- [DMARC with SES](https://docs.aws.amazon.com/ses/latest/dg/send-email-authentication-dmarc.html)
- [Header fields SES changes](https://docs.aws.amazon.com/ses/latest/dg/header-fields.html)
