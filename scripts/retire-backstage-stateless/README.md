# Stateless retirement results

The protected `retire-backstage-stateless.yaml` workflow runs this command on
reviewed `main`. Keep its confirmation, source binding and first-attempt rules;
a diagnostic result does not authorize a replay or clearing an operation journal.

Admission DELETE probes carry `dryRun: [All]` in their `DeleteOptions` body.
Kubernetes reads options from a nonempty DELETE body instead of its query string;
the URL's `dryRun=All` alone does not prevent deletion. For an older attempt that
used query-only probes, `HOLD_ADMISSION` does not establish that no deletion was
submitted. Inspect its existing journal and resources without replaying it.

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
