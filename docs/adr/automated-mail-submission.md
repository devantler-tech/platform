# Automated mail submission

**Status:** Proposed

## Context

The business portal needs invitation messages with one enforced company sender.
Application credentials must not grant mailbox access or unrestricted submission.
An optional upstream content hook cannot be the only boundary: the application
must remain restricted when that hook is absent or unavailable.

## Decision

Use a private SMTP submission component between the application and company Email.
It authenticates the application over TLS, validates the configured envelope and
single message author, and forwards synchronously over verified upstream TLS.
Its application credential differs from the upstream send-only credential and is
not accepted by the upstream service. No successful acknowledgement is returned
before the upstream accepts the complete message.

The executable has no default listener. It is not installed or enabled by this
source delivery. Deployment, private credential custody and real delivery remain
under the dedicated-sender issue. Existing mail, inbound routing and the agents'
restricted sender are unchanged; no external relay or subscription is introduced.

## Alternatives

- A scoped native DATA hook can reject invalid authors, but omission can skip it.
  Using it alone would make application restriction depend on optional upstream
  configuration and a separate activation hold.
- Replacing the company mail server would expand the work into mailbox migration
  and recovery. That is unnecessary for an application submission boundary.

## Consequences

The component adds one maintained process and separate private credentials. It
needs bounded concurrency, sizes, deadlines, authentication attempts and rate
limits, with no message or credential logging. It performs no background retry.
A lost upstream acknowledgement can leave delivery uncertain; callers must use
bounded retries and replay-safe invitation links rather than infer non-delivery.

Loopback protocol tests use a separately authenticated SMTP peer and generated
certificates. They are not deployed Stalwart, external delivery, SPF/DKIM/DMARC,
customer enrollment or portal-activation proof. Those remain explicit parent gates.
