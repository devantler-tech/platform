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
// Each theme carries a fingerprint derived from its kind, key, and the SORTED
// set of affected components. It deliberately excludes counts, timestamps,
// resource versions and UIDs, so:
//
//   - unchanged cluster state yields a byte-identical fingerprint across runs
//     (the anti-churn guarantee), and
//   - a fluctuating severity COUNT updates an existing entry rather than
//     re-filing a new one.
//
// # Input must come from per-object GETs — this is not a style preference
//
// Kubescape's storage apiserver returns SPEC-STRIPPED skeletons on LIST:
// `kubectl get vulnerabilitymanifestsummaries -A -o json` reports every
// severity as 0 and every control map as null, while a `kubectl get <crd>
// <name> -n <ns> -o json` of THE SAME OBJECT returns the real values.
// Measured on prod 2026-08-01: one workload read critical=0/high=0/medium=0
// via LIST and critical=18/high=69/medium=82 via GET.
//
// A bridge fed from LIST output therefore sees a spotless cluster and files
// nothing, forever, while looking healthy — the exact "a broken scanner and a
// compliant one read identically" failure this whole program exists to
// prevent. So the parser FAILS CLOSED on that shape rather than reporting a
// clean bill of health: see errStrippedList and the -min-hydration-sample
// flag.
//
// Usage, from the repository root:
//
//	go run ./scripts/kubescape-backlog-bridge -posture posture.json -cve cve.json
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// errStrippedList reports input carrying the spec-stripped LIST shape. It is a
// hard error, never a warning: the whole point is that this shape is
// indistinguishable from a clean cluster to any caller that does not check.
var errStrippedList = errors.New("input looks like a spec-stripped Kubescape LIST")

// errWritesNotEnabled reports the default-off half being requested.
var errWritesNotEnabled = errors.New("issue writes are not enabled in this slice")

const (
	// themePosture groups a failed posture control across every workload it
	// affects.
	themePosture = "posture"
	// themeCVE groups a CVE severity class across every workload carrying it.
	themeCVE = "cve"

	// defaultMinHydrationSample is how many objects must be present before an
	// entirely-empty input is treated as the stripped-LIST signature rather
	// than a genuinely clean (or genuinely tiny) input. A handful of empty
	// objects is ordinary; hundreds of uniformly empty ones is the apiserver
	// stripping them.
	defaultMinHydrationSample = 10
)

// item is the subset of a Kubescape summary object this command reads. Fields
// outside it are ignored on purpose, so an upstream schema addition does not
// break the bridge.
type item struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		// Controls is the posture surface: controlID -> control state.
		Controls map[string]struct {
			ControlID string `json:"controlID"`
			Severity  struct {
				Severity string `json:"severity"`
			} `json:"severity"`
			Status struct {
				Status string `json:"status"`
			} `json:"status"`
		} `json:"controls"`
		// Severities is the CVE surface. Kubescape spells it two ways: a bare
		// integer on the posture summaries and a {"all": N} object on the
		// vulnerability summaries, so it is decoded loosely and normalised by
		// severityCount.
		Severities map[string]json.RawMessage `json:"severities"`
	} `json:"spec"`
}

// list is a `kubectl get ... -o json` document.
type list struct {
	Items []item `json:"items"`
}

// theme is one drainable backlog entry: a control or CVE class plus every
// public component it affects.
type theme struct {
	Kind       string   // themePosture | themeCVE
	Key        string   // control ID, or severity class
	Severity   string   // human-readable severity, for the title
	Components []string // sorted "namespace/kind/name" — the sanitized minimum
	Count      int      // occurrences; deliberately NOT part of the fingerprint
}

// Fingerprint is the theme's stable identity across runs.
//
// It covers the kind, the key, and the sorted component set — and nothing
// else. Counts, timestamps, resource versions and UIDs are excluded so that
// unchanged state re-derives the same value and a count change updates rather
// than re-files.
func (t theme) Fingerprint() string {
	canonical := t.Kind + "|" + t.Key + "|" + strings.Join(t.Components, ",")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}

// Title renders the backlog title. Stable for a given theme so a
// search-before-create can match on it.
func (t theme) Title() string {
	switch t.Kind {
	case themePosture:
		return fmt.Sprintf("security(posture): %s fails on %s", t.Key, pluralComponents(len(t.Components)))
	default:
		return fmt.Sprintf("security(cve): %s-severity findings on %s", t.Key, pluralComponents(len(t.Components)))
	}
}

func pluralComponents(n int) string {
	if n == 1 {
		return "1 workload"
	}
	return fmt.Sprintf("%d workloads", n)
}

// severityCount normalises the two spellings Kubescape uses for a severity
// count: a bare integer, or an object with an "all" member.
func severityCount(raw json.RawMessage) int {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var wrapped struct {
		All int `json:"all"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		return wrapped.All
	}
	return 0
}

// component renders the sanitized public identity of a scanned object:
// namespace, workload kind and workload name. It deliberately omits node
// names, pod IPs, image digests, UIDs and the wlid internals — those are
// reachability evidence that must not reach a public issue.
func (i item) component() string {
	ns := i.Metadata.Labels["kubescape.io/workload-namespace"]
	if ns == "" {
		ns = i.Metadata.Namespace
	}
	kind := i.Metadata.Labels["kubescape.io/workload-kind"]
	name := i.Metadata.Labels["kubescape.io/workload-name"]
	if name == "" {
		name = i.Metadata.Name
	}
	if kind == "" {
		return ns + "/" + name
	}
	return ns + "/" + kind + "/" + name
}

// hydrated reports whether an object carries any finding payload at all. It is
// the per-object half of the stripped-LIST guard.
func (i item) hydrated() bool {
	if len(i.Spec.Controls) > 0 {
		return true
	}
	for _, raw := range i.Spec.Severities {
		if severityCount(raw) > 0 {
			return true
		}
	}
	return false
}

// checkHydration fails closed when the whole input carries the stripped-LIST
// signature: many objects, not one of them with any payload.
//
// The threshold is what keeps this from firing on a genuinely clean handful of
// objects. It cannot distinguish a stripped LIST from a truly spotless large
// cluster — and that ambiguity is resolved toward failing, because a false
// stop is a loud, fixable error while a false all-clear is the silent failure
// this bridge exists to prevent.
func checkHydration(items []item, minSample int) error {
	if len(items) < minSample {
		return nil
	}
	for _, it := range items {
		if it.hydrated() {
			return nil
		}
	}
	return fmt.Errorf("%w: all %d objects carry zero severities and no controls; "+
		"Kubescape's apiserver strips these on LIST, so feed this command per-object "+
		"`kubectl get <crd> <name> -n <ns> -o json` output instead of `kubectl get <crd> -A -o json`",
		errStrippedList, len(items))
}

// acc accumulates one theme's affected components while scanning items. Both
// surfaces share it, so they also share assemble's determinism guarantees.
type acc struct {
	severity   string
	components map[string]struct{}
}

func (a *acc) add(component string) { a.components[component] = struct{}{} }

// severityRank orders Kubescape's severity names. Unknown names rank lowest so
// an upstream addition cannot silently outrank Critical.
func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 5
	case "high":
		return 4
	case "medium":
		return 3
	case "low":
		return 2
	case "negligible":
		return 1
	default:
		return 0
	}
}

// raiseSeverity keeps the highest severity seen for a theme.
//
// One control can be reported at different severities by different workloads,
// and Go randomises map iteration — so taking whichever arrived first would
// make this field depend on iteration order. Keeping the maximum is
// order-independent, which is what every other field here already guarantees.
func (a *acc) raiseSeverity(s string) {
	if severityRank(s) > severityRank(a.severity) {
		a.severity = s
	}
}

// derivePosture groups failed posture controls into themes.
func derivePosture(items []item) []theme {
	byControl := map[string]*acc{}
	for _, it := range items {
		for id, ctrl := range it.Spec.Controls {
			// Only failures are backlog work. "passed" and "skipped" are not
			// findings, and filing them is the issue-spam failure mode.
			if !strings.EqualFold(ctrl.Status.Status, "failed") {
				continue
			}
			key := id
			if ctrl.ControlID != "" {
				key = ctrl.ControlID
			}
			a, ok := byControl[key]
			if !ok {
				a = &acc{severity: ctrl.Severity.Severity, components: map[string]struct{}{}}
				byControl[key] = a
			}
			a.raiseSeverity(ctrl.Severity.Severity)
			a.add(it.component())
		}
	}
	return assemble(themePosture, byControl)
}

// deriveCVE groups CVE counts into one theme per severity class.
func deriveCVE(items []item) []theme {
	bySeverity := map[string]*acc{}
	for _, it := range items {
		for class, raw := range it.Spec.Severities {
			if severityCount(raw) == 0 {
				continue
			}
			a, ok := bySeverity[class]
			if !ok {
				a = &acc{severity: class, components: map[string]struct{}{}}
				bySeverity[class] = a
			}
			a.add(it.component())
		}
	}
	return assemble(themeCVE, bySeverity)
}

// assemble turns the accumulator map into sorted, deterministic themes.
//
// Every collection is sorted before it leaves this function: Go randomises map
// iteration, so without this the fingerprints would differ run to run on
// identical input — which would defeat the entire anti-churn guarantee.
func assemble(kind string, m map[string]*acc) []theme {
	out := make([]theme, 0, len(m))
	for key, a := range m {
		comps := make([]string, 0, len(a.components))
		for c := range a.components {
			comps = append(comps, c)
		}
		sort.Strings(comps)
		out = append(out, theme{
			Kind:       kind,
			Key:        key,
			Severity:   a.severity,
			Components: comps,
			Count:      len(comps),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "kubescape-backlog-bridge: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("kubescape-backlog-bridge", flag.ContinueOnError)
	fs.SetOutput(out)
	posturePath := fs.String("posture", "", "path to per-object workloadconfigurationscansummary JSON")
	cvePath := fs.String("cve", "", "path to per-object vulnerabilitymanifestsummary JSON")
	mode := fs.String("mode", "report", "report (default, prints what it would file) or write (not enabled yet)")
	minSample := fs.Int("min-hydration-sample", defaultMinHydrationSample,
		"objects required before an entirely-empty input is treated as a spec-stripped LIST")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *mode != "report" {
		if *mode != "write" {
			return fmt.Errorf("unknown -mode %q (want report or write)", *mode)
		}
		return fmt.Errorf("%w: this slice ships the report-only half of #2854; "+
			"the issue-writing half lands behind the same flag once fingerprint "+
			"stability is demonstrated on consecutive runs", errWritesNotEnabled)
	}

	if *posturePath == "" && *cvePath == "" {
		return errors.New("no input: pass -posture and/or -cve. " +
			"Reporting \"nothing to file\" for an empty invocation would be the same " +
			"false all-clear the stripped-LIST guard exists to prevent")
	}

	var themes []theme
	if *posturePath != "" {
		items, err := readList(*posturePath)
		if err != nil {
			return err
		}
		if err := checkHydration(items, *minSample); err != nil {
			return fmt.Errorf("posture input: %w", err)
		}
		themes = append(themes, derivePosture(items)...)
	}
	if *cvePath != "" {
		items, err := readList(*cvePath)
		if err != nil {
			return err
		}
		if err := checkHydration(items, *minSample); err != nil {
			return fmt.Errorf("cve input: %w", err)
		}
		themes = append(themes, deriveCVE(items)...)
	}

	return report(themes, out)
}

func readList(path string) ([]item, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var l list
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return l.Items, nil
}

// report prints the themes this run would file, newest surface first. The
// output is deterministic so two consecutive runs on unchanged state are
// byte-identical — that equality IS the acceptance check.
func report(themes []theme, out io.Writer) error {
	if len(themes) == 0 {
		_, err := fmt.Fprintln(out, "no live-only findings — nothing to file")
		return err
	}
	for _, t := range themes {
		if _, err := fmt.Fprintf(out, "%s\t%s\tseverity=%s\t%s\tcomponents=%s\n",
			t.Fingerprint(), t.Kind, t.Severity, t.Title(), strings.Join(t.Components, ",")); err != nil {
			return err
		}
	}
	return nil
}
