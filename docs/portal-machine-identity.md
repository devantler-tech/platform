# Portal machine identity

The customer Dex is prepared with a confidential client `business-portal-agent`
for portal automation. Its client-credentials grant is disabled in production.
The client has no browser redirect and no cross-client trust. It is separate
from the `business-portal` browser client and represents no human user.

The native vault-seed Password generator creates its credential once in a durable
Kubernetes Secret. Its create-only PushSecret stores it at
`apps/customer-identity/portal-agent-client`, property `client-secret`, in OpenBao.
An ExternalSecret projects that value into customer Dex through
`CUSTOMER_PORTAL_AGENT_CLIENT_SECRET`. The portal needs only the issuer's public
signing keys. Agents must never read, print, copy or transmit the credential or
production tokens. A future caller receives the credential through its own
reviewed native workload projection.

After a separate reviewed activation, request `grant_type=client_credentials`
and `scope=openid` at the issuer's token endpoint. The explicitly activated native
fixture of the pinned Dex distribution returns both `access_token` and `id_token`
as signed JWTs with these claims:

| Claim | Value |
| --- | --- |
| `iss` | `https://login.devantler.tech` |
| `aud` | `business-portal-agent` (one string) |
| `sub` | `ChVidXNpbmVzcy1wb3J0YWwtYWdlbnQ` |
| `exp - iat` | 300 seconds |

The subject encodes the client ID and an empty connector. The browser sign-in
verifier must refuse that subject. Dex's grant list is global: after activation,
the browser client's credential can also obtain client-credentials tokens, but those have the
browser audience and its own empty-connector subject. Neither token belongs to
the portal's machine allow-list or its human sign-in flow. Refresh tokens and
`offline_access` are unavailable for client credentials.

## Activation and acceptance

The production grant list contains only `authorization_code`, so both the
browser and machine clients are refused client-credentials issuance. Preparing
the static client and its credential does not enable that grant or portal API
access.

Activation is tracked separately in [#6448](https://github.com/devantler-tech/platform/issues/6448).
It requires a reviewed Git change after a signed business-site
release supporting the strict machine verifier and rejecting empty-connector
subjects in browser sign-in is deployed and verified. The supporting source is
[business-site #665](https://github.com/devantler-tech/business-site/pull/665);
source merge alone does not establish deployment. Only then enable the Dex grant
and add the following non-secret settings to the platform's portal-settings
projection, starting with no write grants:

```dotenv
PORTAL_AGENT_AUDIENCE=business-portal-agent
PORTAL_AGENT_SUBJECTS=[{"issuer":"https://login.devantler.tech","subject":"ChVidXNpbmVzcy1wb3J0YWwtYWdlbnQ","grants":[]}]
```

The native regression exercises the exact first-party image selected by the
customer Dex HelmRelease with disposable synthetic credentials and memory
storage. It first proves the actual production grant configuration refuses both
clients with `unsupported_grant_type` and emits no tokens. Only after that proof
passes does it add `client_credentials` to a copied configuration in memory for
an explicitly activated test-only fixture. That fixture verifies both
signatures, the identity and lifetime of both tokens, wrong-credential and
offline-access refusals, the browser client's separate identity, and rejection
of a browser redirect for the machine client:

```sh
CUSTOMER_AGENT_NATIVE_TEST=1 go test -count=1 -v ./scripts/tests/customer-agent
```

Production acceptance still requires normal protected GitOps deployment,
metadata-only readback of generation, PushSecret and projection readiness, and
an approved native verifier that obtains a token internally and calls the live
portal API without exposing credentials, tokens, or response data to logs.
Verify a read succeeds, an ungranted write fails, and browser/machine credentials
cannot cross either authentication boundary. Synthetic native proof does not
establish this production outcome.

## Pinned source

The deployed image's build record names Dex revision
`75eb19238e10fc4750e7efb05a4031b254b04029`. Its
[client-credentials handler](https://github.com/dexidp/dex/blob/75eb19238e10fc4750e7efb05a4031b254b04029/server/grants/clientcredentials.go)
uses the client ID with an empty connector. Its
[token issuer](https://github.com/dexidp/dex/blob/75eb19238e10fc4750e7efb05a4031b254b04029/server/tokens/issuer.go)
signs both token types with the configured ID-token lifetime. The
[claims implementation](https://github.com/dexidp/dex/blob/75eb19238e10fc4750e7efb05a4031b254b04029/server/tokens/claims.go)
encodes that subject and uses the requesting client as the sole audience when no
cross-client scope is requested.
