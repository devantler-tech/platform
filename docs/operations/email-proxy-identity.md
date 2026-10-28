# Prepared Email client transport

The optional `private-mail-proxy-identity` bundle prepares a maintained Caddy
adapter for the existing Email administration route. It remains absent from
production. Activation and availability proof belong to #5448 and #5412.

The Gateway replaces the client address header with connection metadata. The
adapter accepts one canonical address from that trusted path, removes HTTP
identity headers and sends a PROXY v2 envelope to a dedicated local Stalwart
listener. Backend connections are never reused between clients. Stalwart keeps
its ordinary access controls and evaluates each transported client address.

This adapter is needed because the pinned Gateway controller does not emit an
upstream PROXY envelope, while Stalwart evaluates connection bans before HTTP
forwarding headers. A listener-scoped local override avoids an address-wide
allowed-IP exception. The adapter receives no credential, mailbox or TLS-key
volume; Cilium limits the frontend to the Gateway identity and explicitly fences
native and recovery endpoints. The address check in Caddy supplements
that identity policy and cannot replace it.

The configuration helper's `useProxyClientIP` setting defaults to false and
cannot be combined with `useForwardedClientIP`. It declares the local listener's
trusted set explicitly in both directions, so replaying false clears a previous
enabled setting. The unused local listener remains declared during rollback;
it is never an externally exposed endpoint.

Before activation, require current-head native Caddy/Stalwart and pinned Envoy
proof, correct real-client identity through the deployed provider, refusal of
forged/ambiguous/direct untrusted requests, unchanged retained access rules,
authenticated administration and sustained readiness. The production source
must pin the newly published signed image, opt into the bundle and settings,
and coordinate the provider/receiver transport settings. The prepared bundle
requests direct DNS through its existing declarative owner. No browser DNS
change is part of this preparation.

Rollback restores the explicit disabled provider and receiver settings, selects
the administration bundle and replays `useProxyClientIP: false`. Retain data,
credentials and client access rules throughout. A successful source test is not
production identity or recovery evidence.

References: [Caddy's maintained upstream PROXY transport](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#the-http-transport),
[Stalwart's listener-scoped trust](https://www.stalw.art/docs/server/reverse-proxy/proxy-protocol/),
[pinned Gateway cluster construction](https://github.com/cilium/cilium/blob/v1.20.3/operator/pkg/model/translation/envoy_cluster.go).
