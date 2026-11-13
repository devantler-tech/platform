# Native message fixture

`zitadel-v4.20.0.json` contains fictional output from the unmodified
`internal/notification/messages.Email.GetContent` implementation at ZITADEL
commit `8afa5dd37a8e723efb9eace8d09fd5c1cccef694`. That source file's SHA-256 is
`94715fb2a0262576a132f5e7f08353b9d12e7fee73df344c4ce241dcdb25c73a`.

The inputs are the fixture's one recipient, sender, sender name, subject and
body, with empty CC, BCC and reply address. The native method supplies the date
and header ordering. JSON encoding preserves the native CRLF and empty-header
whitespace; the protocol test sends it through the real SMTP DATA writer. Regenerate it by
calling that native method in the pinned source module, rather than replacing
the headers with a hand-written approximation.

This fixture proves message-format compatibility. It does not prove deployment,
certificate trust in the native runtime, invitation issuance or real delivery.
