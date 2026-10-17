# ARC pre-activation evidence

## Decision

The protected ARC verification entrypoint has separate transport and App identity
modes. Both are manual, main-only, bounded, and serialized with production
deployment. Neither activates the runner pool or changes access.

Transport verification checks agreement between reviewed and live configuration,
the declared listener's CA and service hostname, and an unauthenticated health
response that explicitly reports initialized and unsealed. Healthy standby is
allowed; sealed and uninitialized status codes are never overridden. This mode
does not request a reader token, log in, read the App entry, sign a JWT, or contact
GitHub. It emits a distinct transport outcome.

App identity verification retains its separate confirmation and verifies the
existing key and organization installation only after transport checks. Actual
same-node isolation evidence and successful stored-key verification remain
separate activation requirements. A transport success satisfies neither.

## Rationale

An operator needs to diagnose listener readiness before granting short-lived
reader access. Combining transport health with stored-key verification creates a
dependency cycle when diagnosing that transport. Separate modes make the weaker
evidence useful without presenting it as credential identity or activation proof.

## Validation and recovery

Local TLS/API fixtures exercise success, malformed and incomplete observations,
rejected certificates, unhealthy responses, and the absence of authentication
operations. They do not prove production health. The production transport mode
must be observed against the deployed listener before its acceptance issue closes.
Failure leaves the staged pool inactive; the verifier is opt-in and makes no
resource or permission changes.
