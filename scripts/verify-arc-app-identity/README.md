# Protected ARC App identity proof

Run `go run ./scripts/verify-arc-app-identity --preflight` from the repository
root to inspect the reviewed credential-transport configuration without any
credentials, cluster access or network requests. An encrypted declaration yields
`TRANSPORT_CONFIG_READY`; this is **not** an identity or runtime transport proof.

The manual **Verify ARC App Identity** workflow accepts only main, the first
attempt, and the exact confirmation `verify-arc-app-identity`. It uses the existing
`prod` environment and deployment concurrency lock. It builds and tests before
restoring existing production access, and stops before that access when the
declared ARC SecretStore lacks verified HTTPS transport.

On a protected invocation, the verifier requires agreement between the reviewed
and live bootstrap App client ID and SecretStore. It accepts a reviewed CA bundle
or a namespaced ConfigMap CA reference. It verifies the real listener's certificate
and service hostname through an unchanged TLS session in a loopback port-forward
**before** requesting a five-minute token for the existing dedicated reader. It
does not create the service account, change a policy or use a broader reader.

The reader authenticates only to the declared Kubernetes auth mount and reads
only the existing ARC App entry. The key stays in process memory. Its RS256 JWT
authenticates two GitHub reads: the App and its organization installation. Both
must match the stored App/installation IDs, the production client ID and the
organization; the installation must be unsuspended with runner write and metadata
read permission. The temporary OpenBao token is revoked even on failure. The
verifier makes no GitHub writes or installation token, registers no runner and
changes no credential or permission.

Outputs contain only `ARC_APP_IDENTITY=<outcome>`. Missing transport or reader
identity, an unavailable entry, mismatches, failed responses and failed cleanup
all return nonzero. Underlying errors, IDs, keys, tokens and response bodies are
never printed or uploaded. Redirects, HTTP, certificate bypasses and environment
proxies are unsupported. Requests and response sizes are bounded.

The current staged store uses HTTP, so its expected preflight is
`HOLD_TRANSPORT`. WireGuard's cross-node encryption or a verifier-only tunnel does
not prove the actual SecretStore's same-node transport. The transport owner must
first deliver authenticated HTTPS and its trusted CA to the listener **and**
SecretStore. This verifier neither enables that store nor clears the other
[ARC activation gates](../../docs/operations/arc-runners.md). The separately owned
activation integration must require a current successful identity proof before
using credentials or activating the pool; this manual workflow is not itself a
deployment hook.

Offline validation:

```sh
go test -race ./scripts/verify-arc-app-identity
go vet ./scripts/verify-arc-app-identity
```

Fixtures use synthetic keys and local TLS servers. They prove the verifier's
behavior, including possession of the fixture signing key, without proving that
the production entry exists or has the correct identity. The protocol follows
[GitHub's App JWT requirements](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app)
and [App/organization installation reads](https://docs.github.com/en/rest/apps/apps).
