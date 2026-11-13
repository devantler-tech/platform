# Email sending address

**Status:** Proposed

## Context

Company Email delivers directly to receiving mail servers (#5481); the
maintainer rejected a hosted relay. A pod's outbound connections leave with the
address of whichever node it runs on. That address changes when the pod moves
or a node is replaced, and it has no reverse DNS for the mail host name, so
receivers would treat the mail as suspect.

On 2026-11-03 the maintainer approved one dedicated, movable IPv4 address and
its recurring cost. Hetzner blocks outbound SMTP per account, and the protected
no-message check has already reached an external mail server on port 25 from a
cluster node ([proof](../dr/email-provider-smtp-proof.md)). So the account permits
direct delivery; what is missing is a stable sending address.

## Decision

- **One Hetzner Floating IPv4** in the cluster's location is the only source
  of Email's outbound mail. Its reverse DNS names the configured mail host, and
  that name's address record points back to it. SPF authorizes this address,
  Email signs with DKIM, and DMARC aligns on the mailbox domain.
- **A Cilium egress-gateway policy** routes only the Email server's
  internet-bound traffic through one gateway node, with the Floating IP as the
  source. Other workloads keep their current egress. The policy covers IPv4
  destinations only, so a gateway without IPv6 stays selectable.
- **The gateway is one static worker**, selected by a single node label.
  Hetzner accepts the Floating IP as a source only from the server it is
  assigned to, so it is assigned to that worker. Cilium requires the address
  on a network device, so every worker carries it on a dummy interface rather
  than on its main interface. The node address the kubelet reports and the
  address Cilium masquerades other traffic to must stay the server's own; the
  implementing revision proves both, and pins the kubelet's node address if
  needed.
- **No high availability.** If the gateway node is lost, Cilium drops the
  matching traffic instead of sending it from another address, and Email's
  queue retries the messages. Moving the gateway means labelling another static
  worker and reassigning the address through a reviewed main-only workflow. No
  in-cluster controller or cloud token is added.
- The address and host name stay in private configuration. Public manifests
  read them from the platform's encrypted substitution variables.

## Order

1. The maintainer creates the Floating IPv4. This is the only account action
   and the only new cost.
2. A reviewed main-only workflow assigns it to the gateway worker, sets its
   reverse DNS and reads both back.
3. A separate revision enables the egress gateway in Cilium, which restarts
   the agents and operator. Existing egress must be unchanged afterwards.
4. Add the dummy-interface address to workers, label the gateway and apply the
   Email policy. First confirm that KSail's pinned Talos configuration library
   accepts the dummy-interface document. Afterwards, confirm that every node's
   reported address and other pods' egress source are unchanged.
5. From the Email server's own network identity, repeat the no-message SMTP
   check. Confirm that the external server sees the Floating IP as the source.
6. Publish the host's address record and the SPF, DKIM and DMARC records
   through the DNS owner, after the maintainer approves them. MX still moves
   only after migration and recovery are verified.

## Alternatives

- **A hosted relay:** rejected by the maintainer.
- **A dedicated mail node with its own address:** costs a whole server, which
  is not authorized.
- **A controller that moves the address automatically:** adds a component and
  an in-cluster cloud token. Static workers are rarely replaced, and Email's
  queue tolerates a manual move.
- **The nodes' own addresses:** they change when nodes are replaced, so they
  cannot be published in SPF or given stable reverse DNS.

## Consequences

Cilium applies an egress-gateway policy to a new pod after a short delay.
Until then, that pod's traffic can leave with its node's address, so a message
sent in that window can fail SPF. Activation measures that window after an
Email server restart, and the outcome decides whether outbound delivery needs
a start-up delay. A gateway outage only delays outbound mail. Inbound mail
continues to use the existing load balancer and is unaffected by this
decision.

Egress gateway is incompatible with Cluster Mesh and CiliumEndpointSlice;
neither is enabled. Replies from the gateway are unencrypted only with XDP
acceleration, which is not enabled either.

## Sources

- [Cilium egress gateway](https://docs.cilium.io/en/stable/network/egress-gateway/egress-gateway/)
- [Hetzner Floating IP configuration](https://docs.hetzner.com/cloud/floating-ips/persistent-configuration/)
- [Hetzner outbound mail policy](https://docs.hetzner.com/cloud/servers/faq/#why-can-i-not-send-any-mails-from-my-server)
- [Talos dummy link configuration](https://docs.siderolabs.com/talos/v1.13/reference/configuration/network/dummylinkconfig)
