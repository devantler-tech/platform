# Automated mail submission

This internal SMTP component constrains an application's automated mail before
submitting it to company mail. It is not a mailbox, delivery queue or relay
subscription. Nothing starts without an explicit command and private inputs.

The application authenticates to this component over implicit TLS. Only its
configured envelope sender and one matching `From` author are accepted. The
component authenticates separately to the upstream SMTP service, using mandatory
STARTTLS with an explicit CA and server name. The application credential must
have no upstream account. Upstream credentials must belong to a dedicated,
send-only identity; deployment must verify that restriction on the real MTA.

Success means the upstream service acknowledged the final DATA command. It does
not mean external delivery. A lost acknowledgement returns a temporary failure;
the application must tolerate possible duplicates and use stable invitation
identifiers. Permanent upstream submission refusals remain permanent failures.
No protocol body, recipient, password or peer error is logged.

## Private configuration

Build from the repository root with `go build -o <output> ./scripts/mail-submission`.
Use `mail-submission check --config-dir <directory>` to validate inputs without
starting a listener. `mail-submission serve --config-dir <directory>` starts the
explicitly configured listener. There is no default address, credential, sender
or development mode.

The directory contains these regular files, owned by root or the service user,
with mode `0600`, `0400`, `0640` or `0440`:

- `settings.json`: one JSON object with string members `listenAddress`, `sender`,
  `senderName`, `clientUser`, `upstreamAddress`, `upstreamServerName`,
  `upstreamUser` and optional `replyTo`.
- `client-password` and `upstream-password`: distinct printable secrets of
  32–256 bytes, without a trailing newline. Usernames must also be distinct.
- `server.crt` and `server.key`: the receiving certificate chain and private key.
- `upstream-ca.pem`: only PEM CA certificates for the upstream trust boundary.

Unknown or duplicate JSON members, unresolved substitutions, malformed trust,
leaf symlinks and unsafe file permissions are refused. Kubernetes Secret
projections are read through one anchored `..data` generation. Rotate inputs by
restarting the component; existing processes intentionally retain their selected
generation. Do not put actual settings, passwords or certificates in Git.

`listenAddress` is an explicit numeric IP and unprivileged port. The server
requires TLS 1.2 or newer, bounds active connections to 16 and each connection to
30 seconds, accepts at most three authentication attempts per connection and one
recipient per message. Messages are limited to 1 MiB and headers to 64 KiB.
Authentication has a shared burst allowance of sixteen attempts and replenishes\none allowance per second. Submission has a shared burst allowance of ten messages and replenishes one
allowance every six seconds. RSET does not reset these budgets. The upstream
connection has an absolute ten-second lifetime. Shutdown allows five seconds for
active sessions, then closes them.

A missing `replyTo` refuses all Reply-To headers. When configured, exactly one
matching address is required. Sender, Return-Path and Resent headers are refused.
The original accepted message bytes are submitted unchanged.

## Validation and deployment boundary

Run `go test -race ./scripts/mail-submission` and `go vet ./scripts/mail-submission`.
Tests use generated fictional certificates and native Go SMTP peers on loopback.
They cover author and envelope refusals, credential separation, receiving and
upstream TLS, unavailable peers, permanent refusals, lost acknowledgements,
resource bounds, private input validation and credential replacement.

These peers are not the production MTA. This change creates no Deployment,
listener in the live platform, mailbox, real credential or external message.
Issue #5400 retains deployment, credential custody, protected recovery,
upstream send-only policy and actual invitation-delivery acceptance. Its
prerequisite recovery hold must remain intact. The portal remains closed until
its separate end-to-end sign-in and invitation checks pass.

The SMTP and SASL libraries are MIT-licensed (`github.com/emersion/go-smtp` and
`github.com/emersion/go-sasl`). The repository's existing terms apply to this
component's own source; no new license grant is introduced.
