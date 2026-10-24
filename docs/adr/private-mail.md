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

- A dedicated subdomain with DNS changes through its owning repository, usable
  TLS, MX, SPF, DKIM and DMARC, leaving the existing mail domain intact.
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

The public daily GitHub ask channel stays the accepted channel. Deploying mail
does not enable spend stewardship or designate its financial decision channel.
Those remain separate maintainer-owned decisions.

## Sources

- [Pinned release](https://github.com/stalwartlabs/stalwart/releases/tag/v0.16.27)
- [Pinned community license](https://github.com/stalwartlabs/stalwart/blob/v0.16.27/LICENSES/AGPL-3.0-only.txt)
- [Declarative deployments](https://stalw.art/docs/configuration/declarative-deployments/)
- [Kubernetes deployment and restricted security context](https://stalw.art/docs/cluster/orchestration/kubernetes/)
- [Hetzner outbound mail policy](https://docs.hetzner.com/cloud/servers/faq/#why-can-i-not-send-any-mails-from-my-server)
