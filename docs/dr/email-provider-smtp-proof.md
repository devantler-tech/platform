# Email provider SMTP proof

Run **Verify Email Provider SMTP** on reviewed `main`, with confirmation
`verify-email-provider-smtp`, once the Email serving image has converged.
Do not rerun a failed attempt before investigating its fixed failure stage.

The source check accepts the administration profile and its named client proxy.
Every container must use the reviewed serving image and be ready, with an image
identity and valid restart count. All identities are bound before and after the
probe; API list order is immaterial. Unknown or duplicate components are refused.

For an operator's source-only evaluation, set
`EMAIL_PROVIDER_READONLY_SOURCE_CONTEXT` to an existing read-only production
context and run `go test -run TestServingSourceLiveReadOnly -count=1
./scripts/verify-email-provider` from the repository root. This reads the serving
pod, controller and node twice. It creates no resource and performs no SMTP
session; ordinary test runs skip it.

The workflow holds the production deployment lock. It reads the ready Email
pod, its StatefulSet and its provider node, then schedules one restricted,
credential-free diagnostic pod on that same node. A temporary network policy
permits DNS and TCP port 25 to the existing external mailbox provider only.
The client uses EHLO with a reserved diagnostic hostname, NOOP and QUIT. It
does not authenticate, supply a sender or recipient, upload a body, or send mail.

The public result contains no address, node name, SMTP banner or credentials.
Success requires the native client to finish successfully, an exact success
marker, unchanged Email pod/controller/node/image/restart bindings, and observed
deletion of both temporary resources. Cleanup uses recorded API UIDs with
server-side deletion preconditions. A successor is never deleted. An uncertain
create or delete remains unverified; retain its private runner receipt while
investigating, and do not guess ownership from a matching name or label.

The driver cleans up on ordinary failure or cancellation. An always-running
workflow step repeats UID-bound cleanup after interruption. Forceful runner
loss can still leave resources behind; every future invocation refuses any
earlier diagnostic resources. Its Pod deadline bounds client execution, but
does not substitute for verified resource deletion.

This proves hosting-node SMTP connectivity to one existing provider. It does
not prove a stable sending address, reverse DNS, Email application egress,
submission or IMAP access, sender authentication, delivery, or mailbox
migration. Those remain separate acceptance gates on #5481.
