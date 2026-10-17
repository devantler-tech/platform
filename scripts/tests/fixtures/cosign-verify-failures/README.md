# Verification diagnostic fixtures

These are classifier inputs, not end-to-end Sigstore service tests. Transport
and mixed-error cases are representative diagnostics, not complete recordings
of a pinned cosign invocation. They exercise the reported error phrases and
their precedence; they do not prove which lookups cosign attempted, suppressed,
or satisfied using fallback trust material.

The rejection cases pin the complete identity-mismatch phrases. The invalid
signature case shares their `no matching` prefix and must remain unrecognised.
Every failure still stops verification regardless of its diagnostic class.
