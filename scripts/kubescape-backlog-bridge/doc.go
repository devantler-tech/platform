// Command kubescape-backlog-bridge turns live-only Kubescape findings into a
// stable, deduped set of themed backlog entries.
//
// Live-only findings — posture controls that only fail once workloads are
// running, and CVEs that only exist against a pulled image — never reach the
// backlog on their own: the engineer's survey is GitHub-only, so they are
// filed exactly when a human happens to notice. This command is the ingestion
// half that closes that gap (#2854, second slice of #2451).
//
// # Modes
//
// `-mode=report` (the default) prints the themes it would file and writes
// nothing. `-mode=write` reconciles them into GitHub issues in `-repo`, which
// is the default-off half: reporting stays the default, so enabling writes is
// an explicit, reversible step rather than something a run falls into.
//
// # Reconciliation
//
// Write mode lists the issues this command already owns — everything carrying
// the `kubescape-bridge` label — matches them to derived themes by fingerprint,
// and then creates, updates, reopens or closes:
//
//   - a theme with no tracked issue is CREATED,
//   - a theme whose issue is closed is REOPENED, never re-created, so a
//     recurring finding lands back on its original issue instead of minting a
//     duplicate,
//   - a theme whose rendered title and body already match its issue writes
//     NOTHING — unchanged cluster state performs zero writes, which is the
//     anti-churn guarantee, and
//   - a tracked issue with no matching theme is CLOSED, but only under the
//     conditions below.
//
// Ownership is established by the LABEL, never by searching issue bodies for
// the fingerprint. Free-text search ranks and stems, so it both returns issues
// that merely mention a marker and omits ones that carry it; a reconciler fed
// that list closes issues it does not own and re-files ones it does.
//
// # Closing and updating are gated, because this command cannot see the cluster
//
// It has no inventory: it cannot tell a whole-cluster sweep from a single
// per-object GET. So a partial run must never conclude that the findings it was
// not shown are resolved. `-inputs-complete` is the caller's explicit assertion
// that EVERY object on each supplied surface was passed, and two writes need it:
//
//   - CLOSING, further scoped to the surfaces this run actually examined — a
//     posture-only run never closes a CVE entry.
//   - UPDATING, because a theme derived from a subset lists only that subset's
//     components. Writing it over a tracked entry drops the components this run
//     never looked at and can lower a recorded count or severity while those
//     findings are still live, which reads to a human as remediation.
//
// CREATING is not gated: a theme nobody tracks yet is new information, and an
// under-scoped new entry is corrected by the first complete run. REOPENING is
// likewise never withheld — a live finding left closed is the worse error — but
// under partial input it reopens carrying the entry's already-filed text rather
// than the subset render.
//
// Both withheld counts are printed, so a gated run never renders as a clean one.
//
// The same asymmetry decides the fail-closed direction throughout: an entry
// whose surface cannot be read is left alone, because a stale open issue costs
// far less than a live finding marked resolved.
//
// # Accepted is not the same as gone
//
// A control every occurrence of which a declared ClusterSecurityException covers
// leaves the derivation exactly as a remediated one does — absent from the
// themes — but the opposite thing is true of the cluster: it is still failing,
// and someone decided to accept that. Such an entry closes with its own wording
// naming the exception, and as GitHub's "not planned" rather than "completed",
// so the structured state a board filters on agrees with the comment posted
// beside it. For the same reason `-mode=write` over posture input REQUIRES
// `-exceptions`: without it every accepted control is derived as live and filed
// as backlog work.
//
// # Two limits the forge imposes
//
// A body over 65,536 characters is refused, and applyPlan stops at the first
// failure — so one broadly-failing control would block every remaining action in
// the run, not merely its own issue. Bodies therefore enumerate at most 300
// components; the stated count is always the true one. The bound is a component
// COUNT rather than a measured byte budget, because a budget would shift as the
// surrounding prose is reworded and re-render every tracked issue.
//
// `gh issue create --label` associates an existing label rather than creating
// one, so EVERY label a create applies — the `kubescape-bridge` ownership label
// and `security` — is provisioned once per run before the first create.
// Provisioning only some of them would fail the create just as surely as
// provisioning none, so the two lists come from one declaration and cannot
// drift. Without that, the first run against a repository this command has not
// seen before files nothing at all.
//
// # Bodies carry the sanitized minimum
//
// Control or CVE class, the affected components as namespace/kind/name, and the
// counts. Node names, pod IPs, image digests, UIDs and wlid internals are
// excluded upstream by component.String and nothing in rendering reintroduces
// them: these issues are PUBLIC artifacts, and detailed reachability evidence
// belongs in the private operator notes instead.
//
// # Themes, not resources
//
// A finding is grouped into a THEME — one per failed posture control, one per
// CVE severity class — never one per resource. A per-resource bridge floods
// the backlog; a themed one produces a drainable queue.
//
// # Fingerprints
//
// Each theme carries a fingerprint derived from its SURFACE and KEY, and
// nothing else. Affected components, their count, totals, timestamps, resource
// versions and UIDs are all excluded, so:
//
//   - unchanged cluster state yields a byte-identical fingerprint across runs
//     (the anti-churn guarantee),
//   - a fluctuating severity COUNT updates an existing entry rather than
//     re-filing a new one, and
//   - a workload joining or leaving a theme likewise updates it, rather than
//     minting a new identity and stranding the old entry.
//
// The affected components and counts are still ACCUMULATED and RENDERED, just
// never fingerprinted: an entry that cannot show 1 critical becoming 999, or a
// second workload picking up the same failed control, is not worth updating.
//
// # Every input is identified structurally, never assumed
//
// Kubescape's storage apiserver returns SPEC-STRIPPED skeletons on LIST. A
// bridge fed from LIST output sees a spotless cluster and files nothing,
// forever, while looking healthy — the exact "a broken scanner and a compliant
// one read identically" failure this whole program exists to prevent.
//
// Two properties of that skeleton make it detectable by SHAPE rather than by a
// size heuristic, and both were measured against prod on 2026-08-01:
//
//   - a posture LIST carries `spec.controls: null`, while a per-object GET
//     carries the control map even when every control PASSED (2215/2215 LIST
//     objects null; a clean GET object carried one passed control);
//   - a CVE LIST blanks `spec.vulnerabilitiesRef` to empty strings, while a
//     per-object GET names the manifest it references — including for an image
//     with zero findings (121/121 LIST objects blank; a critical=0 GET object
//     still named `ghcr.io-cloudnative-pg-...`).
//
// So "examined and empty" and "never examined" are distinguishable per object,
// at any input size. That distinction is this command's core safety property:
// an unexamined object is a hard error, never zero findings and a clean exit.
//
// The same structural markers identify WHICH surface a document belongs to, so
// a CVE document passed to -posture is rejected instead of quietly deriving no
// controls and exiting 0.
//
// Usage, from the repository root. Both flags are repeatable, and EVERY object
// must be passed: one -posture file per workload, and one -cve file per IMAGE.
//
// The CVE side is not one file per workload. A workload running more than one
// image has one summary PER IMAGE, and they are aggregated — measured against
// the live cluster, 117 summaries across 93 workloads, with 22 workloads
// carrying two or three. Passing a single file for such a workload silently
// undercounts its vulnerabilities, and where the omitted image held the only
// findings it produces exactly the false all-clear this command exists to
// prevent. Nothing detects the omission: this command has no cluster
// inventory, so it cannot know an object was never handed to it.
//
//	go run ./scripts/generate-kubescape-exceptions -o exceptions.json
//	go run ./scripts/kubescape-backlog-bridge \
//	  -exceptions exceptions.json \
//	  -posture ns-a-workload.json -posture ns-b-workload.json \
//	  -cve ns-a-workload-image-1.json -cve ns-a-workload-image-2.json
//
// The same invocation reconciling issues, once a sweep really did pass every
// object — `-inputs-complete` is what authorises closing and updating, so it
// belongs only on a run that can honestly make that claim, and `-exceptions` is
// required rather than optional as soon as posture input is being written:
//
// Note that the file list is GENERATED from the sweep directory rather than
// written out by hand. Naming two files beside -inputs-complete would make a
// completeness claim the invocation visibly does not meet, and that claim is
// what authorises closing and updating — so an example that could be copied
// into a partial run is the destructive outcome the gate exists to prevent:
//
//	sweep=./kubescape-sweep   # one full pass, written by the collection step
//
//	args=()
//	for f in "$sweep"/posture/*.json; do args+=(-posture "$f"); done
//	for f in "$sweep"/cve/*.json; do args+=(-cve "$f"); done
//
//	go run ./scripts/kubescape-backlog-bridge \
//	  -mode write -repo devantler-tech/platform -inputs-complete \
//	  -exceptions exceptions.json "${args[@]}"
package main
