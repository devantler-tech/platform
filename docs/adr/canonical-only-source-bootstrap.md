# Canonical-only source bootstrap

Status: Accepted

## Decision

A canonical-only consumer has a distinct, qualified source binding. It has no
legacy signing history and must never acquire an invented legacy approval row.
The initial profile admits only the business site's immutable source-only Tenant.
Its application reconciliation, WebApp capability, external DNS and SOPS options
are explicitly disabled.

The binding identifies the consumer repository, source declaration, complete
constructor specification, Tenant identity, generated source identity, manifest
digest and complete publication tuple. The publication tuple includes the source
tag/commit, publisher repository/workflow/commit, caller workflow, event, run and
attempt. The existing publication-evidence receipt remains verification-only.
Creating an admission candidate requires independent publication verification;
the reviewed candidate grants only the declared source bootstrap.

Raw discovery and production conservation independently validate the complete
constructor and instance. Both project the same exact OCI source contract before
comparing complete inventories. An unknown, altered, cloned or duplicated source
constructor or instance refuses the census; it cannot disappear beside approved
ordinary consumers. Discovery buffers results until the complete observation is
validated.

Legacy and legacy-plus-canonical consumers retain their current tables, matcher
generation and observation semantics. The canonical-only registry is separate.
The writer copies an already pinned Tenant unchanged and validates its staged
constructor/binding context. It never modifies the shared constructor or creates
a legacy subject for this consumer. Regeneration includes the consumer in its
read-only token scope; its write token remains restricted to Platform.

## Observation and recovery

A `published-bootstrap` record with no observation row permits a complete live
inventory to establish that the exact source is absent or pending without an
artifact. It emits no applied-observation row. A current, uniquely identified,
Ready and SourceVerified source with matching generation and artifact revision
retires bootstrap authority through the supported regeneration transition.

After a `published-binding` record and its observation row exist, missing,
pending, malformed, duplicated or unreadable live evidence fails regeneration.
It never erases the observation or restores bootstrap authority. Conflicting
record/observation states fail closed. Historical observation is not current
readiness, and a failed read is not evidence of absence.

## Delivery boundary

Source registration uses the normal reviewed production pipeline and fixed
read-only controller/source acceptance. Workload, private-input, route and
identity activation remain separate acceptance. No paid service or customer
enrollment is introduced by source-only admission.
