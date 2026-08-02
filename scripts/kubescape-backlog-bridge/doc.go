// Command kubescape-backlog-bridge turns live-only Kubescape findings into a
// stable, deduped set of themed backlog entries.
//
// Live-only findings — posture controls that only fail once workloads are
// running, and CVEs that only exist against a pulled image — never reach the
// backlog on their own: the engineer's survey is GitHub-only, so they are
// filed exactly when a human happens to notice. This command is the ingestion
// half that closes that gap (#2854, second slice of #2451).
//
// # Report-only
//
// This slice ships the read/derive half ONLY. `-mode=report` (the default)
// prints the themes it WOULD file; `-mode=write` is the default-off half and
// is refused with a non-zero exit until the issue-writing slice lands. That is
// the feature-flag-first shape: the code lands latent and the flip is a
// separate, reversible step.
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
package main
