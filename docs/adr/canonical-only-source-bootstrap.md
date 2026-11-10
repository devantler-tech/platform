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

## Source-only upgrades

An existing source cannot use first-admission bootstrap again. An explicitly
prepared `published-upgrade` binds a separately verified replacement publication
and retains the previous binding plus a hashed, current source observation.
Preparation validates complete discovery and the exact source verification
constraint, observes the source twice, and rechecks the declaration, constructor
and admission before creating a new private output directory. It never changes
the checkout or cluster and never expands the approved publisher set.

Raw and rendered discovery project the proposed digest. Until it is applied,
regeneration accepts only the previously verified source with its recorded UID
hash and generation, and preserves the previous observation row or its absence.
It never labels the proposal as applied. Current-generation Ready/SourceVerified
evidence for the proposed digest permits ordinary regeneration to replace the
transitional record with `published-binding` and its actual observation row.
Missing or inconsistent evidence never restores bootstrap authority. Both
application flags, external DNS and SOPS remain disabled throughout this path.

The maintained preparation command generates the admission and Tenant changes
together for normal review. A failed output reservation cannot overwrite an
existing directory or final symlink. Interrupted partial output is not a valid
preparation result; only a successful complete invocation may be copied for review.

## Publisher transitions

The consumer's canonical approvals may contain one or two distinct immutable
publisher revisions. The current source binding still names exactly one genuine
publisher; a staged approval never changes the attribution of existing bytes or
expands the current source's verification matcher. Both the current and proposed
publishers must remain approved while a transition retains its previous binding.
Unknown families, floating references, duplicates and a third revision are refused.

Publication verification derives the publisher from the authenticated release
run and binds that same identity to the immutable caller, certificate, signed
claims, receipt and final run readback. Preparation writes the replacement digest
and exact publisher revision together, rechecking the approval snapshot before
emitting its private candidate. It never edits the committed current binding.

A changed main-branch pin without a matching proposed publication fails
regeneration without changing any output. A valid proposed publication preserves
the complete previous observation until the replacement digest and its exact
publisher constraint are actually observed Ready and SourceVerified. Existing
application and enrollment restrictions remain unchanged.

## Delivery boundary

Source registration uses the normal reviewed production pipeline and fixed
read-only controller/source acceptance. Workload, private-input, route and
identity activation remain separate acceptance. No paid service or customer
enrollment is introduced by source-only admission.
