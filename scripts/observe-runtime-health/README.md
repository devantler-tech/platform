# Read-only runtime health observation

After this code is reviewed and merged, dispatch `observe-runtime-health.yaml`
once from `main`, with confirmation `observe-runtime-health`. It follows the
existing production diagnostic workflow: no more than `contents: read`, no
application or policy mutation, and ephemeral credentials restored only after
the observer is built. The program independently refuses a different repository,
event, ref, retry, confirmation or unbound source revision.

The observer reads the fixed Coroot service and the project named `platform`,
then the Kubescape rule and binding APIs. It prints aggregate facts only: no
workload names, messages, risk details, addresses or credentials. Invalid or
incomplete responses and transport errors produce `UNVERIFIED` and exit 2.
The application inventory must be nonempty; an empty or absent response cannot
become a clean snapshot. The dedicated alerts query propagates database errors
that the overview's context otherwise hides.

Application issues, log-error **patterns**, and risks refer to a fixed one-hour
window. Absolute request timestamps use Coroot's epoch-millisecond convention.
Window edges match its whole-second resolution, including in the reported snapshot.
Log-pattern counts are application-audit findings, not a complete raw-log
event count. Alert counts are current observations; the firing total retains
the greater of the dedicated query and the per-severity overview peaks during
collection. The unresolved total includes the dedicated query's complete count,
independently of its one-entry detail limit. Dismissed risks remain in the total,
and unknown statuses are reported separately.

Rule counts cover the Kubescape namespace; binding counts cover cluster-scoped
bindings. References resolve by ID, exact name or tags, in the released
node-agent's precedence order. The chart's mismatched `Port forward` name does
not resolve to the `Port forward to pod` definition. Ambiguous names are unknown.
The rule readback proves object presence and references only. It
does not trigger a detector or prove delivery, namespace coverage or complete
runtime security. `full_platform_acceptance_verified` deliberately remains false:
this diagnostic is one evidence source, never a platform all-clear.

Run `go test ./scripts/observe-runtime-health` without cluster credentials for
response, failure, target-discovery and dispatch-boundary controls.
