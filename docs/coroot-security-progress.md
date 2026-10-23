# Security progress in Coroot

The security dashboards answer three questions: what remains, whether the evidence is
complete, and how the remaining work changes over time. They do not close issues, change
security thresholds or create exceptions. A falling count is a net change in observed
findings: fixes, deleted workloads, exclusions and inventory changes can all affect it.
Confirm an individual closure against the resource and its delivery issue.

The optional `security-progress` Coroot component provisions three dashboards:

- **Platform security progress** leads with source collection, coverage and unresolved work.
- **Kyverno remaining policy findings** separates failures, errors, warnings and skipped
  checks, then shows policy, rule and affected resource legends.
- **Kubescape remediation progress** separates failed posture checks, accepted risks,
  incomplete scan coverage and full-image vulnerability occurrences.

Coroot Community Edition 1.22.2 supports time-series panels. Resource legends lead
with the resource name; image legends lead with severity. Hover over a legend label
to read its complete identity when the visible text is shortened. Coroot chooses
series colours; use labels and values to assess status. Then inspect the source report and its
existing remediation issue. The dashboards describe these limits and link the existing
security roadmap. They are not a resource findings table or an issue tracker.

## Trust the evidence before reading a score

The collector polls every five minutes. It uses the same denominator-aware
[`check-kubescape-result-coverage.sh`](../scripts/check-kubescape-result-coverage.sh)
as the operational checks, including by-name storage reads and checks for inconsistent
vulnerability summaries. Its optional `--snapshot-dir` retains a validated input snapshot
in a private temporary directory. The collector reduces that snapshot to metrics and
deletes it. No raw scan documents or credentials enter the metrics endpoint.

Collection failure replaces the old source's counts with an explicit failed collection
signal. Dashboard queries also require a recent collection timestamp. Missing data is
never filled with a healthy zero. If no usable posture or vulnerability result remains,
its finding counts are absent while coverage gaps stay visible. Runtime learning, partial profiles, missing results,
stale vulnerabilities and storage-version disagreements remain visible.

Posture result coverage does **not** establish posture scan age: the current result
objects lack a trustworthy scan-completion timestamp. The freshness-observable indicator
therefore remains zero. Complete runtime profiles likewise do **not** prove that a
detector is active or that an alert reached its destination; the runtime-delivery
indicator stays unverified. These are evidence gaps, not failing scores to suppress.

Vulnerability totals count full-image CVE occurrences once per current image digest and
severity, using current, coherent summaries. The same CVE in two images counts twice.
They do not claim unique CVEs across the platform, runtime relevance or complete coverage
while images are missing or stale. Source severities and acceptance markers are retained;
an accepted rule cannot turn an otherwise failed control into an accepted control.
Kyverno reports include legacy `kyverno` results and the newer generating, mutating
and validating policy families. Results are deduplicated by source, policy, rule and
resource; the source stays in drill-down legends so equal names in different policy
families cannot collide. Policy-level CEL results without a rule use `(policy)`.
Unrecognised Kyverno families fail collection instead of silently disappearing.
Skipped checks remain separate from passes. Observed reports alone do not prove
complete policy execution.

## Policy Reporter replacement decision

Coroot can host policy trends and remaining-finding drill-down series. This component
reads Kyverno PolicyReport resources directly, so these dashboards do not depend on
Policy Reporter's metrics exporter. This is sufficient for an overview and prioritising
remaining work, but it does not reproduce Policy Reporter's report messages, exception
views, policy metadata or dedicated resource findings UI.

Keep the Policy Reporter service while evaluating those detail workflows. Retiring its
UI, plugin or core is a separate change after observed parity and a consumer inventory;
dashboard availability alone is not replacement evidence. The current service, API,
metrics and route are preserved by this component.

## Deployment and maintenance

The shared base leaves this component **off by default**. The production infrastructure
overlay explicitly enables
`../../../bases/infrastructure/coroot/components/security-progress` in its `components`.
To disable collection and reconciliation, remove that production component reference
through the protected merge/deploy path. In the same change, remove
`Deployment/coroot-security-progress` from the Coroot budget's reviewed-unrelated list
in `scripts/pdb-selector-exceptions.tsv`, since it no longer renders alongside the
operator-managed Coroot server. Also remove the `ClusterRole/coroot-security-progress`
and `ClusterRoleBinding/coroot-security-progress` entries from
`scripts/validate-eks-ci-role-policy/approved-surface.txt`; the authorization validator
rejects approvals for resources that no longer render. To re-enable the component,
restore its production reference and the reviewed-unrelated entry, then review the
rendered RBAC and restore both matching authorization fingerprints in the same
protected change. Run the deployment validation before either transition. The
collector needs only the explicitly listed read-only resources; only its container
receives a projected service-account token. Exec probes check publication freshness,
project binding and the two local services. Reporting a failed security source still
counts as a functioning collector; it makes the dashboards show degraded evidence.
Metrics are served on loopback and sent by
an in-pod Prometheus agent to Coroot's existing metrics backend. There is no Service or
new public endpoint.

The reconciliation CronJob runs every 15 minutes, repairs only the three owned
dashboards, refuses name/ownership collisions and does nothing when configuration
already agrees. Unrelated dashboards remain editable and untouched. Edit the versioned
definitions to keep managed dashboards changed; direct UI edits are reconciled away.
Disabling the component stops future reconciliation and collection, while the dashboards
remain as recoverable Coroot state and their collection signals age out.

Every snapshot resolves the named Coroot project through its existing internal API
and attaches its `coroot_project_id` to every metric. Coroot adds this selector to
queries against its shared backend; unlabelled metrics would be invisible to the
dashboards. Dynamic discovery also recovers from a restored or recreated project.
Failed or ambiguous project discovery publishes no finding counts.

Source scripts, queries and JSON live beside the component. After changing any source
or the shared coverage checker/reviewed exclusion list, regenerate its ConfigMap:

```bash
go run ./scripts/generate-coroot-security-config
go test ./scripts/tests/security-progress-metrics ./scripts/tests/coroot-security-dashboards
bash scripts/tests/test-check-kubescape-result-coverage.sh
ksail workload validate
ksail --config ksail.prod.yaml workload validate
```

CI checks generated/source parity and the enabled and disabled renderings. The
`Validate Coroot Security Progress` workflow exercises the pinned helper images and
the released Coroot binary against synthetic data. It reconciles twice, verifies
every persisted panel and preserves browser screenshots and panel responses as
seven-day artifacts. Inspect those screenshots before accepting a change to the
dashboard layout. These fixtures establish dashboard behaviour; they do not establish
production coverage or platform health.

After deployment, observe successful collection and reconciliation, open all three
dashboards in Coroot, exercise resource legends and a longer time range, and verify
that incomplete evidence remains visible. Keep #5260 open until that live evaluation
passes; static validation alone is not delivery.
