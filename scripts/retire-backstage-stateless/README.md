# Stateless retirement results

The protected `retire-backstage-stateless.yaml` workflow runs this command on
reviewed `main`. Keep its confirmation, source binding and first-attempt rules;
a diagnostic result does not authorize a replay or clearing an operation journal.

Admission DELETE probes carry `dryRun: [All]` in their `DeleteOptions` body.
Kubernetes reads options from a nonempty DELETE body instead of its query string;
the URL's `dryRun=All` alone does not prevent deletion. For an older attempt that
used query-only probes, `HOLD_ADMISSION` does not establish that no deletion was
submitted. Inspect its existing journal and resources without replaying it.

After the controller-owned lifecycle has completed, the separate protected
`complete-backstage-retirement.yaml` workflow can remove the durable namespace
journal. It is not a retry path. The completion command accepts only the exact
terminal state: the captured stateless identities are absent, retained Helm
history is uninstalled and is read from the exact Kubernetes Secret UID captured
in the journal rather than depending on Helm to rediscover a terminal release,
the operation receipt and journal agree, both database
instances and continuous archiving are healthy, reciprocal Retain volumes are
unchanged, and the production apps root is blocked by that journal alone. Its
single JSON Patch tests the namespace UID, resourceVersion and exact journal
value before removing only that annotation. It then waits for current Flux
readiness and repeats the retained-data proof twice.

The normal current-main CD and main merge-group deployment paths have one
read-only handoff for this terminal state. If Flux reconciliation fails, either
may continue only after the exact journal, receipt, absent stateless targets,
healthy database, retained storage and journal-only `apps` block are stable
across two samples. It additionally
requires the journal's source to be an ancestor of current main and every change
since then to belong to the fixed terminal-maintenance implementation; a
deployable manifest change refuses the handoff. The handoff makes no cluster
mutation and does not apply to any other Flux failure. Its successful, attested
deployment receipt authorizes the separately protected
completion workflow to repeat the proof and remove the journal. The normal CD
path is run again after completion, when all application checks can bind to the
newly applied revision.

A completion refusal is always nonzero and names only its bounded stage:

```text
PORTAL-RETIREMENT=HOLD_COMPLETION stage=evidence-first
```

An already-cleared, partial, foreign or changed state is a refusal, never
success or permission to broaden the write. Successful completion reports:

```text
PORTAL-RETIREMENT=COMPLETED journal=REMOVED preservationSamples=2 flux=READY dataRetirement=UNASSESSED
```

A preflight refusal exits nonzero and identifies the failing stage, for example:

```text
PORTAL-RETIREMENT=HOLD_PRECONDITION stage=evidence-second
```

The fixed stage names distinguish source freshness (`source-opening`,
`source-before-journal`), read-only assessment (`assessment`), the two evidence
collections (`evidence-first`, `evidence-second`), eligibility checks
(`eligibility-first`, `eligibility-second`), captured targets (`targets-first`,
`targets-second`), sample separation (`sample-separation`), snapshot stability
(`snapshot-second`), retained-object preservation (`preservation-first`,
`preservation-second`) and the unused-operation check (`journal-unused`).

Both execution and the read-only `--check` mode use these stages. Preservation
failures found inside eligibility retain the preservation stage and still stop
at that early check; an earlier owner-eligibility failure takes precedence.
Preservation refusals carry one allow-listed coarse category, such as
`category=claim-ownership` or `category=storage-retention`; a changed retained
projection between samples reports `category=sample-change`. Unrecognized
errors report `category=unknown` rather than exposing their text.

Eligibility refusals also carry a fixed category, such as `category=owner-profile`
or `category=deployment-source`. Only exact, locally authored errors are mapped;
unknown or private errors remain `category=unclassified`, with a nonzero exit.
This identifies the current refusing boundary, not the cause of a historical
attempt, and does not authorize a retry. Output contains no raw command errors,
resource payloads or credentials. All existing checks and post-submission
lifecycle results remain in force.

New namespace journals are written with the dedicated `backstage-retirement`
field manager so application reconciliation does not own their lifecycle. This
writer change does not transfer an existing journal or authorize its removal;
existing terminal maintenance remains separately guarded. The native kubectl
regression exercises the full guarded entrypoint and checks the real PATCH
manager, unchanged journal binding, and namespace UID/resource-version tests.
