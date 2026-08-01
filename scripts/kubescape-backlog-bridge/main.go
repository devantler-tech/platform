// Command entry point: flag parsing, input reading, and theme derivation.
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
	"slices"
	"sort"
	"strings"
)

// errStrippedList reports input carrying the spec-stripped LIST shape. It is a
// hard error, never a warning: the whole point is that this shape is
// indistinguishable from a clean cluster to any caller that does not check.
var errStrippedList = errors.New("input looks like a spec-stripped Kubescape LIST")

// errWritesNotEnabled reports the default-off half being requested.
var errWritesNotEnabled = errors.New("issue writes are not enabled in this slice")

// errUnrecognisedDocument reports input that is neither a kubectl list nor a
// single Kubescape object.
var errUnrecognisedDocument = errors.New("input is neither a kubectl list nor a Kubescape object")

// errSurfaceMismatch reports a document handed to the wrong scan surface — a
// CVE summary passed to -posture, or the inverse. Without this the wrong file
// derives no themes and exits 0, which is a false all-clear reached by a typo.
var errSurfaceMismatch = errors.New("input does not belong to the requested scan surface")

// surface is the Kubescape scan surface a document belongs to.
type surface string

const (
	// surfacePosture groups a failed posture control across every workload it
	// affects.
	surfacePosture surface = "posture"
	// surfaceCVE groups a CVE severity class across every workload carrying it.
	surfaceCVE surface = "cve"
)

// component is the sanitized public identity of a scanned object. It
// deliberately omits node names, pod IPs, image digests, UIDs and the wlid
// internals — those are reachability evidence that must not reach a public
// issue.
//
// It is kept STRUCTURED rather than pre-joined because exception designators
// match on the individual fields.
type component struct {
	Namespace string
	Kind      string
	Name      string
}

// String renders the component. A cluster-scoped object has no namespace, and
// is rendered with an explicit "cluster" scope marker rather than an empty
// leading segment, so a reader cannot mistake it for a missing value.
func (c component) String() string {
	scope := c.Namespace
	if scope == "" {
		// Angle brackets cannot occur in an RFC 1123 namespace name, so this
		// marker cannot collide with a real namespace literally called
		// "cluster" — which would otherwise render identically to cluster scope.
		scope = "<cluster>"
	}

	if c.Kind == "" {
		return scope + "/" + c.Name
	}

	return scope + "/" + c.Kind + "/" + c.Name
}

// control is one posture control's state within a summary object.
type control struct {
	ControlID string `json:"controlID"`
	Severity  struct {
		Severity string `json:"severity"`
	} `json:"severity"`
	Status struct {
		Status string `json:"status"`
	} `json:"status"`
}

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
		// Controls is decoded lazily so that an ABSENT key, an explicit null,
		// and a present map stay distinguishable — that is what separates a
		// stripped LIST object from a genuinely all-passed one.
		Controls json.RawMessage `json:"controls"`
		// Severities is the CVE count surface. Kubescape spells it two ways: a
		// bare integer on the posture summaries and a {"all": N} object on the
		// vulnerability summaries, so it is decoded loosely and normalised by
		// severityCount.
		Severities map[string]json.RawMessage `json:"severities"`
		// VulnerabilitiesRef is present-but-blanked on a stripped LIST and
		// names a real manifest on a per-object GET, including for an image
		// with zero findings.
		VulnerabilitiesRef *struct {
			All struct {
				Name string `json:"name"`
			} `json:"all"`
		} `json:"vulnerabilitiesRef"`
	} `json:"spec"`
}

// surface reports which scan surface this object belongs to, structurally.
//
// The two surfaces are disjoint by key: only a posture summary carries
// `controls`, and only a vulnerability summary carries `vulnerabilitiesRef`.
// Both carry `severities`, so that field cannot be the discriminator.
func (i item) surface() (surface, bool) {
	if i.Spec.Controls != nil {
		return surfacePosture, true
	}

	if i.Spec.VulnerabilitiesRef != nil {
		return surfaceCVE, true
	}

	return "", false
}

// examined reports whether the apiserver actually returned this object's
// payload, as opposed to the stripped skeleton a LIST returns.
//
// This is deliberately NOT "does it have findings": an all-passed control map
// and a zero-severity image are examined and empty, which is a legitimate
// clean result. Only the absence of the payload STRUCTURE means unexamined.
func (i item) examined() bool {
	switch {
	case i.Spec.Controls != nil:
		// A stripped posture object carries `controls: null`. `{}` is a real
		// answer — examined, nothing evaluated.
		return !isJSONNull(i.Spec.Controls)
	case i.Spec.VulnerabilitiesRef != nil:
		// A stripped CVE object blanks the reference; a real one names the
		// manifest even when every severity is zero.
		return i.Spec.VulnerabilitiesRef.All.Name != ""
	default:
		return false
	}
}

// isJSONNull reports whether a raw message is the literal null.
func isJSONNull(raw json.RawMessage) bool {
	return string(trimJSONSpace(raw)) == "null"
}

func trimJSONSpace(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}

// controls decodes the posture control map. An absent or null map decodes to
// nothing; callers gate on examined() first.
func (i item) controls() (map[string]control, error) {
	if i.Spec.Controls == nil || isJSONNull(i.Spec.Controls) {
		return nil, nil
	}

	var m map[string]control
	if err := json.Unmarshal(i.Spec.Controls, &m); err != nil {
		return nil, fmt.Errorf("decode spec.controls: %w", err)
	}

	return m, nil
}

// theme is one drainable backlog entry: a control or CVE class plus every
// public component it affects.
type theme struct {
	Kind       string   // string(surfacePosture) | string(surfaceCVE)
	Key        string   // control ID, or severity class
	Severity   string   // human-readable severity, for the title
	Components []string // sorted "namespace/kind/name" — the sanitized minimum
	Count      int      // affected components; deliberately NOT fingerprinted
	Total      int      // summed occurrences (CVEs); deliberately NOT fingerprinted
}

// Fingerprint is the theme's stable identity across runs.
//
// It covers the surface and the key, and NOTHING else — not the affected
// components, their count, totals, timestamps, resource versions or UIDs.
//
// The component set is deliberately excluded. A theme is "this control fails"
// or "this severity class is present"; which workloads exhibit it is mutable
// state ABOUT that theme, not part of its identity. Including it meant one
// workload starting or stopping to exhibit an unchanged theme minted a new
// identity, so the write path would re-file the theme and strand the old entry
// — exactly the churn excluding the count was meant to prevent, since the
// component set implicitly encodes that count anyway.
func (t theme) Fingerprint() string {
	canonical := t.Kind + "|" + t.Key
	sum := sha256.Sum256([]byte(canonical))

	return hex.EncodeToString(sum[:])[:16]
}

// Title renders the backlog title. Stable for a given theme so a
// search-before-create can match on it.
func (t theme) Title() string {
	switch t.Kind {
	case string(surfacePosture):
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
//
// It reports ok=false for anything else rather than returning zero. Returning
// zero would make corrupt or changed scanner output indistinguishable from a
// clean result — the same false all-clear this command exists to prevent, one
// level down. Note `null` decodes into an int as a silent no-op success, so it
// is rejected explicitly rather than caught by the decode error.
func severityCount(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return 0, false
	}

	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}

	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return 0, false
	}

	all, ok := wrapped["all"]
	if !ok || isJSONNull(all) {
		return 0, false
	}

	var n2 int
	if err := json.Unmarshal(all, &n2); err != nil {
		return 0, false
	}

	return n2, true
}

// component renders the sanitized public identity of a scanned object.
//
// The workload namespace comes ONLY from the label, never from the summary CR's
// own metadata.namespace. Those are different things: the CR is stored in the
// scanner's namespace, so falling back to it fabricates a namespace for every
// cluster-scoped object.
//
// That fallback was never correct. Measured over the live cluster's 2215 posture
// summaries: 1808 carry the label (1652 of them equal to metadata.namespace, 156
// deliberately different), and all 407 that do not are cluster-scoped kinds —
// ClusterRoleBinding, PersistentVolume, Namespace, Node, webhook configurations.
// So the fallback fired only where it invented an answer, and it assigned them
// the scanner's namespace, which a namespace-scoped ClusterSecurityException
// then matched — silently suppressing real cluster-scoped findings.
//
// A cluster-scoped component therefore has an EMPTY namespace, which no
// namespace designator matches. If a namespaced object ever arrives without the
// label it also gets an empty namespace, which fails safe: the exception stops
// applying and the finding is kept rather than hidden.
func (i item) component() component {
	c := component{
		Namespace: i.Metadata.Labels["kubescape.io/workload-namespace"],
		Kind:      i.Metadata.Labels["kubescape.io/workload-kind"],
		Name:      i.Metadata.Labels["kubescape.io/workload-name"],
	}
	if c.Name == "" {
		c.Name = i.Metadata.Name
	}

	return c
}

// checkExamined fails closed when ANY object in the input carries the stripped
// skeleton.
//
// It is per-object and therefore size-independent: the previous size heuristic
// held the fail-closed guarantee only on a large cluster and quietly dropped it
// for a namespace-scoped query or a small one.
func checkExamined(items []item) error {
	for _, it := range items {
		if it.examined() {
			continue
		}

		return fmt.Errorf("%w: %s carries no payload structure "+
			"(posture `spec.controls` is null, or CVE `spec.vulnerabilitiesRef` is blank); "+
			"Kubescape's apiserver strips these on LIST, so feed this command per-object "+
			"`kubectl get <crd> <name> -n <ns> -o json` output instead of `kubectl get <crd> -A -o json`",
			errStrippedList, it.component())
	}

	return nil
}

// checkSurface rejects a document handed to the wrong scan surface.
func checkSurface(items []item, want surface) error {
	for _, it := range items {
		got, ok := it.surface()
		if !ok {
			return fmt.Errorf("%w: %s carries neither `spec.controls` nor `spec.vulnerabilitiesRef`",
				errUnrecognisedDocument, it.component())
		}

		if got != want {
			return fmt.Errorf("%w: %s is a %s document but was passed to -%s",
				errSurfaceMismatch, it.component(), got, want)
		}
	}

	return nil
}

// acc accumulates one theme's affected components while scanning items. Both
// surfaces share it, so they also share assemble's determinism guarantees.
type acc struct {
	severity   string
	components map[string]struct{}
	total      int
}

func (a *acc) add(c component) { a.components[c.String()] = struct{}{} }

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
	switch {
	case severityRank(s) > severityRank(a.severity):
		a.severity = s
	case severityRank(s) == severityRank(a.severity) && s < a.severity:
		// Equal ranks still need a deterministic winner: "High" and "HIGH"
		// rank the same, as do two unknown names that both rank zero. Keeping
		// whichever arrived first made the rendered bytes depend on input
		// order, which is precisely what the anti-churn guarantee forbids.
		a.severity = s
	}
}

// derivePosture groups failed posture controls into themes, dropping any
// (control, component) pair a declared ClusterSecurityException already covers.
func derivePosture(items []item, exceptions []exception) ([]theme, error) {
	byControl := map[string]*acc{}

	for _, it := range items {
		ctrls, err := it.controls()
		if err != nil {
			return nil, err
		}

		comp := it.component()

		for id, ctrl := range ctrls {
			// Only failures are backlog work. The other recognised statuses are
			// not findings, and filing them is the issue-spam failure mode.
			//
			// An EMPTY or unrecognised status is neither: skipping it silently
			// would let malformed or changed scanner output read as a clean
			// control. Measured on live data, every control carries one of the
			// recognised values, so an unrecognised one means the input — not
			// the cluster — is wrong.
			failed, known := controlFailed(ctrl.Status.Status)
			if !known {
				return nil, fmt.Errorf("%w: %s reports control %s with status %q, "+
					"which is neither a failure nor a recognised non-failure",
					errUnrecognisedDocument, comp, id, ctrl.Status.Status)
			}

			if !failed {
				continue
			}

			// An empty embedded ID falls back to the map key. But when BOTH are
			// present and disagree, silently preferring the embedded one applies
			// exceptions to a different control than the one keyed — a failed
			// C-9999 carrying controlID C-0036 would be suppressed by a declared
			// C-0036 exception.
			key := id
			if ctrl.ControlID != "" {
				if ctrl.ControlID != id {
					return nil, fmt.Errorf("%w: %s keys a control as %q but its controlID is %q; "+
						"refusing to guess which control an exception should match",
						errUnrecognisedDocument, comp, id, ctrl.ControlID)
				}

				key = ctrl.ControlID
			}

			// An accepted control is not actionable work. Filtering is
			// per-COMPONENT, never per-control: a kind/name-scoped exception
			// must not suppress the same control on a workload it does not
			// name, which would itself be a false all-clear.
			if excepted(exceptions, key, comp) {
				continue
			}

			a, ok := byControl[key]
			if !ok {
				a = &acc{severity: ctrl.Severity.Severity, components: map[string]struct{}{}}
				byControl[key] = a
			}

			a.raiseSeverity(ctrl.Severity.Severity)
			a.add(comp)
			a.total++
		}
	}

	return assemble(surfacePosture, byControl), nil
}

// deriveCVE groups CVE counts into one theme per severity class.
func deriveCVE(items []item) ([]theme, error) {
	bySeverity := map[string]*acc{}

	for _, it := range items {
		comp := it.component()

		// A real per-object CVE response always carries its severity buckets —
		// all six of them, zero-valued when the image is clean (measured live).
		// An absent or empty map is therefore malformed input, not a clean
		// result, and iterating it zero times would report exactly that.
		if missing := missingSeverityBuckets(it.Spec.Severities); len(missing) > 0 {
			return nil, fmt.Errorf("%w: %s is missing severity bucket(s) %s; a genuine CVE summary "+
				"reports all of them, zero-valued when the image is clean, so a truncated set "+
				"could hide findings in the omitted buckets",
				errUnrecognisedDocument, comp, strings.Join(missing, ", "))
		}

		for class, raw := range it.Spec.Severities {
			n, ok := severityCount(raw)
			if !ok {
				return nil, fmt.Errorf("%w: %s reports severity %q in an unrecognised shape; "+
					"expected an integer or {\"all\": N}", errUnrecognisedDocument, comp, class)
			}

			if n < 0 {
				return nil, fmt.Errorf("%w: %s reports severity %q as %d; a count cannot be negative",
					errUnrecognisedDocument, comp, class, n)
			}

			if n == 0 {
				continue
			}

			a, ok := bySeverity[class]
			if !ok {
				a = &acc{severity: class, components: map[string]struct{}{}}
				bySeverity[class] = a
			}

			a.add(comp)
			// The count is summed rather than used as a predicate: 1 critical
			// and 999 criticals must not render identically, or an update to an
			// existing entry carries no information.
			a.total += n
		}
	}

	return assemble(surfaceCVE, bySeverity), nil
}

// assemble turns the accumulator map into sorted, deterministic themes.
//
// Every collection is sorted before it leaves this function: Go randomises map
// iteration, so without this the fingerprints would differ run to run on
// identical input — which would defeat the entire anti-churn guarantee.
func assemble(kind surface, m map[string]*acc) []theme {
	out := make([]theme, 0, len(m))

	for key, a := range m {
		comps := make([]string, 0, len(a.components))
		for c := range a.components {
			comps = append(comps, c)
		}

		sort.Strings(comps)

		out = append(out, theme{
			Kind:       string(kind),
			Key:        key,
			Severity:   a.severity,
			Components: comps,
			Count:      len(comps),
			Total:      a.total,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	return out
}

// pathList collects a repeatable path flag.
//
// A cluster-wide run needs one per-object GET per workload, so a single-valued
// flag made the promised cluster-wide grouping unreachable: repeating it merely
// overwrote the previous value, and the only native bulk document is the
// stripped LIST this command rejects.
type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ",") }

func (p *pathList) Set(v string) error {
	if v == "" {
		return errors.New("path must not be empty")
	}

	*p = append(*p, v)

	return nil
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

	var posturePaths, cvePaths pathList

	fs.Var(&posturePaths, "posture",
		"path to a per-object workloadconfigurationscansummary JSON (repeatable)")
	fs.Var(&cvePaths, "cve",
		"path to a per-object vulnerabilitymanifestsummary JSON (repeatable)")

	mode := fs.String("mode", "report", "report (default, prints what it would file) or write (not enabled yet)")
	exceptionsPath := fs.String("exceptions", "",
		"path to the generated Kubescape exceptions JSON "+
			"(`go run ./scripts/generate-kubescape-exceptions`); accepted posture controls are not backlog work")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Go stops flag parsing at the first non-flag argument, so
	// `-posture a.json b.json -cve c.json` silently drops b.json AND the -cve
	// flag after it, then exits 0 having read only a.json — a partial report
	// from a plausible cluster-wide invocation. Reproduced on live data.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q: each input needs its own flag "+
			"(-posture a.json -posture b.json), because flag parsing stops at the first "+
			"non-flag argument and would silently ignore everything after it", fs.Arg(0))
	}

	if *mode != "report" {
		if *mode != "write" {
			return fmt.Errorf("unknown -mode %q (want report or write)", *mode)
		}

		return fmt.Errorf("%w: this slice ships the report-only half of #2854; "+
			"the issue-writing half lands behind the same flag once fingerprint "+
			"stability is demonstrated on consecutive runs", errWritesNotEnabled)
	}

	if len(posturePaths) == 0 && len(cvePaths) == 0 {
		return errors.New("no input: pass -posture and/or -cve. " +
			"Reporting \"nothing to file\" for an empty invocation would be the same " +
			"false all-clear the stripped-LIST guard exists to prevent")
	}

	exceptions, err := loadExceptions(*exceptionsPath)
	if err != nil {
		return err
	}

	var (
		themes   []theme
		examined []surface
	)

	if len(posturePaths) > 0 {
		postureItems, err := readSurface(posturePaths, surfacePosture)
		if err != nil {
			return err
		}

		derived, err := derivePosture(postureItems, exceptions)
		if err != nil {
			return err
		}

		themes = append(themes, derived...)
		examined = append(examined, surfacePosture)
	}

	if len(cvePaths) > 0 {
		cveItems, err := readSurface(cvePaths, surfaceCVE)
		if err != nil {
			return err
		}

		derived, err := deriveCVE(cveItems)
		if err != nil {
			return err
		}

		themes = append(themes, derived...)
		examined = append(examined, surfaceCVE)
	}

	return report(themes, examined, len(exceptions) > 0, out)
}

// readSurface reads every file for one surface, validating each against that
// surface and against the stripped-skeleton signature before combining them.
func readSurface(paths []string, want surface) ([]item, error) {
	var all []item

	for _, path := range paths {
		items, err := readList(path)
		if err != nil {
			return nil, err
		}

		if err := checkSurface(items, want); err != nil {
			return nil, fmt.Errorf("%s input %s: %w", want, path, err)
		}

		if err := checkExamined(items); err != nil {
			return nil, fmt.Errorf("%s input %s: %w", want, path, err)
		}

		all = append(all, items...)
	}

	return all, nil
}

// readList accepts BOTH shapes kubectl can produce, and rejects anything else.
//
// `kubectl get <crd> -A -o json` emits {"items":[…]}, while a per-object
// `kubectl get <crd> <name> -n <ns> -o json` — the form this command's own
// documentation asks for — emits a BARE object with no "items" key. Decoding
// the bare form into a list silently left Items nil, so a document carrying
// real findings reported "nothing to file" and exited 0. That is the same
// false all-clear the stripped-skeleton guard exists to prevent, reached by
// following the instructions, so the shape is determined explicitly rather
// than assumed.
//
// An explicitly empty {"items":[]} stays a legitimate pass: the query
// unambiguously returned nothing. Anything unrecognisable is a hard error,
// never zero findings and a clean exit.
func readList(path string) ([]item, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied scan output path; this is a CLI argument by design
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if rawItems, ok := probe["items"]; ok {
		// `items: null` decodes into a nil slice WITHOUT error, so it would
		// otherwise pass every guard vacuously and report a clean cluster —
		// the same false all-clear as the stripped skeleton, reached by a
		// different shape. An explicitly empty list is `items: []`.
		if isJSONNull(rawItems) {
			return nil, fmt.Errorf("%w: %s carries `items: null`, which is not an empty result; "+
				"pass the document `kubectl get <crd> -o json` emits (an empty list is `items: []`)",
				errUnrecognisedDocument, path)
		}

		var items []item
		if err := json.Unmarshal(rawItems, &items); err != nil {
			return nil, fmt.Errorf("parse %s: \"items\" is not an array of objects: %w", path, err)
		}

		return items, nil
	}

	// A single object identifies itself by carrying a spec — the only part this
	// command reads. Requiring it keeps an unrelated JSON document from being
	// accepted as an empty scan.
	if _, ok := probe["spec"]; ok {
		var single item
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		return []item{single}, nil
	}

	return nil, fmt.Errorf("%w: %s has neither an \"items\" array nor a \"spec\"; "+
		"pass `kubectl get <crd> -o json` output (list or single object)", errUnrecognisedDocument, path)
}

// report prints the themes this run would file. The output is deterministic so
// two consecutive runs on unchanged state are byte-identical — that equality IS
// the acceptance check. `total` is rendered but never fingerprinted, so a
// changed count updates an entry instead of re-filing one.
func report(themes []theme, examined []surface, filtered bool, out io.Writer) error {
	// A posture report derived WITHOUT the declared exceptions lists controls the
	// platform has already accepted. That is a legitimate view to ask for, but it
	// must not be mistaken for a drainable backlog — so the report says which one
	// it is rather than leaving the reader to infer it from the command line.
	if !filtered && slices.Contains(examined, surfacePosture) {
		if _, err := fmt.Fprintln(out,
			"note: no -exceptions supplied, so declared ClusterSecurityExceptions were NOT applied; "+
				"accepted controls are included below and this output is not a filed-work list"); err != nil {
			return err
		}
	}

	if len(themes) == 0 {
		// Scope the all-clear to what was actually read: the SUPPLIED INPUTS,
		// not the surface. Two claims are being avoided here. A bare "no
		// live-only findings" covers surfaces this invocation never looked at;
		// naming the surface still implies the whole surface was covered, when
		// the caller may have passed a single per-object GET. This command has
		// no cluster inventory, so it cannot claim completeness at all.
		names := make([]string, 0, len(examined))
		for _, s := range examined {
			names = append(names, string(s))
		}

		sort.Strings(names)

		_, err := fmt.Fprintf(out, "no live-only findings in the supplied %s input(s) — nothing to file "+
			"(only the objects passed on the command line were examined; "+
			"this is not a statement about the whole cluster)\n", strings.Join(names, " and "))

		return err
	}

	for _, t := range themes {
		if _, err := fmt.Fprintf(out, "%s\t%s\tseverity=%s\ttotal=%d\t%s\tcomponents=%s\n",
			t.Fingerprint(), t.Kind, t.Severity, t.Total, t.Title(), strings.Join(t.Components, ",")); err != nil {
			return err
		}
	}

	return nil
}

// controlFailed classifies a Kubescape control status.
//
// It returns known=false for an empty or unrecognised value so the caller can
// fail closed. The non-failure set is explicit rather than "anything that is
// not failed": that catch-all silently absorbed a missing status, which is
// exactly how malformed scanner output becomes a clean report.
func controlFailed(status string) (failed, known bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		return true, true
	case "passed", "skipped", "irrelevant", "excluded", "ignored":
		return false, true
	default:
		return false, false
	}
}

// cveSeverityBuckets is the bucket set a genuine vulnerabilitymanifestsummary
// reports — measured against the live cluster, where every per-object response
// carried all six, zero-valued when the image is clean.
//
// (The posture summaries carry a DIFFERENT set of five, without "negligible",
// which is why this is checked only on the CVE surface.)
var cveSeverityBuckets = []string{"critical", "high", "low", "medium", "negligible", "unknown"}

// missingSeverityBuckets reports which expected buckets a document omits.
//
// A non-empty map is not sufficient: a response retaining one bucket while
// truncating another passes a length check, and the omitted bucket's findings
// are simply never seen. Extra buckets are tolerated so an upstream addition
// does not break the run.
func missingSeverityBuckets(got map[string]json.RawMessage) []string {
	var missing []string

	for _, want := range cveSeverityBuckets {
		if _, ok := got[want]; !ok {
			missing = append(missing, want)
		}
	}

	return missing
}
