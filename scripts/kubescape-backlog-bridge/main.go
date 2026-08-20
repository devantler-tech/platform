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
	"math"
	"os"
	"slices"
	"sort"
	"strings"
)

// errStrippedList reports input carrying the spec-stripped LIST shape. It is a
// hard error, never a warning: the whole point is that this shape is
// indistinguishable from a clean cluster to any caller that does not check.
var errStrippedList = errors.New("input looks like a spec-stripped Kubescape LIST")

// errWritesNotEnabled reports the default-off half being requested without the
// configuration it needs.
var errWritesNotEnabled = errors.New("issue writes are not enabled")

// errInvalidInvocation reports a flag combination that is refused before any
// input is read.
//
// It exists so the refusals below can be BOUND BY A TEST. run's early
// validations all fail an invocation that is also missing inputs, so a test
// asserting only "some error came back" passes for whichever check happens to
// run first — and would keep passing if the checks were reordered and the one
// it names stopped firing. errors.Is against this sentinel names the refusal
// the test is actually about.
var errInvalidInvocation = errors.New("invalid invocation")

// errUnrecognisedDocument reports input that is neither a kubectl list nor a
// single Kubescape object.
var errUnrecognisedDocument = errors.New("input is neither a kubectl list nor a Kubescape object")

// errMalformedScanContent reports a document that decoded as a Kubescape
// object but whose content cannot be used — a missing workload identity, a
// blank or unrecognised severity, an undecidable scan surface, a bad count.
// It is its own sentinel for the same reason errDuplicateObject is: the file's
// SHAPE is fine, so reporting it under errUnrecognisedDocument would tell an
// operator their input was unparseable and send them to inspect the document
// rather than the field the message actually names. The discriminator is
// structural — a message that can name the component has already decoded it.
var errMalformedScanContent = errors.New("recognised scan object carries unusable content")

// errSurfaceMismatch reports a document handed to the wrong scan surface — a
// CVE summary passed to -posture, or the inverse. Without this the wrong file
// derives no themes and exits 0, which is a false all-clear reached by a typo.
var errSurfaceMismatch = errors.New("input does not belong to the requested scan surface")

// errSurfaceNotEstablished reports an input whose surface cannot be DETERMINED,
// which is a weaker claim than errSurfaceMismatch and needs its own sentinel for
// exactly that reason: "does not belong" asserts knowledge this case does not
// have. It is also not errMalformedScanContent — there is no scan object to
// carry unusable content — and not errUnrecognisedDocument, because an empty
// list is a perfectly well-formed thing for kubectl to emit. Reporting it as
// any of the three would describe the operator's input as something it is not.
var errSurfaceNotEstablished = errors.New("input's scan surface cannot be established")

// errDuplicateObject reports one scan object supplied more than once. It is its
// own sentinel because the document is perfectly well-formed — reporting it
// under errUnrecognisedDocument would tell an operator their input was
// unparseable and send them to inspect the file rather than their arguments.
var errDuplicateObject = errors.New("the same scan object was supplied more than once")

// surface is the Kubescape scan surface a document belongs to.
type surface string

const (
	// surfacePosture groups a failed posture control across every workload it
	// affects.
	surfacePosture surface = "posture"
	// surfaceCVE groups a CVE severity class across every workload carrying it.
	surfaceCVE surface = "cve"
)

// component is the sanitized public identity of a scanned object. Pod IPs,
// image digests, UIDs and the wlid internals are never carried at all — those
// are reachability evidence that must not reach a public issue.
//
// A node name is the one exception, and the distinction is deliberate: when the
// scanned object IS a Node, its name is also its hostname, and exception
// designators still have to match on it. So the STRUCT keeps the real name and
// String — the only path to a public artifact — pseudonymizes it. Withholding
// happens at the boundary, not in the field.
//
// It is kept STRUCTURED rather than pre-joined because exception designators
// match on the individual fields.
type component struct {
	Namespace string
	Kind      string
	Name      string
}

// nodeIdentityPrefix marks a component name that has been replaced by a
// pseudonym. It is spelled so a reader can tell at a glance that the value is
// deliberately not a hostname.
const nodeIdentityPrefix = "node-"

// nodeIdentityDigestLen is the hex width of a pseudonymized node identity.
// Node counts are cluster-sized, not adversarial: 32 bits over a few dozen
// names leaves collision probability negligible, and a pseudonym only has to
// separate one node from another within a single issue body.
const nodeIdentityDigestLen = 8

// String renders the component. A cluster-scoped object has no namespace, and
// is rendered with an explicit "cluster" scope marker rather than an empty
// leading segment, so a reader cannot mistake it for a missing value.
//
// This is the ONLY place a component becomes a public string — acc.add is its
// single call site, and exception designators match the structured fields
// instead — which is what makes it the right place to enforce the exclusion the
// package documents, rather than a second rule that rendering has to remember.
//
// A `Node` is the one kind whose NAME is itself the reachability evidence the
// package excludes. For every other kind the name is a workload identity and
// the node it runs on never appears; for a Node, `kubescape.io/workload-name`
// IS the hostname, so rendering it verbatim publishes exactly what doc.go,
// renderBody and this type all promise is withheld. Kubescape reports Node
// findings in the live input, so this is a reachable path, not a hypothetical.
//
// It is pseudonymized rather than dropped: the body must still show that N
// distinct nodes are affected and let two entries be told apart. The digest is
// taken over the name alone and is therefore deterministic, so an unchanged
// cluster renders identical bytes and the anti-churn guarantee holds.
func (c component) String() string {
	scope := c.Namespace
	if scope == "" {
		// Angle brackets cannot occur in an RFC 1123 namespace name, so this
		// marker cannot collide with a real namespace literally called
		// "cluster" — which would otherwise render identically to cluster scope.
		scope = "<cluster>"
	}

	name := c.Name
	if c.Kind == "Node" && name != "" {
		digest := sha256.Sum256([]byte(name))
		name = nodeIdentityPrefix + hex.EncodeToString(digest[:])[:nodeIdentityDigestLen]
	}

	if c.Kind == "" {
		return scope + "/" + name
	}

	return scope + "/" + c.Kind + "/" + name
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
		// manifest even when every severity is zero. Trimmed, because a marker
		// of only whitespace names no manifest either — and accepting it would
		// let a document that evaluated nothing pass as hydrated, which under
		// -inputs-complete reads as "every tracked CVE issue is resolved".
		return strings.TrimSpace(i.Spec.VulnerabilitiesRef.All.Name) != ""
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

// Fingerprint is the theme's stable historical lookup key across runs.
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
	return t.Identity()[:16]
}

// Identity is the theme's collision-resistant machine identity.
//
// Fingerprint keeps its historical 64-bit prefix so already-filed issues stay
// addressable. New bodies retain the complete digest as a second marker, which
// lets reconciliation verify that a fingerprint still names the same theme.
func (t theme) Identity() string {
	canonical := t.Kind + "|" + t.Key
	sum := sha256.Sum256([]byte(canonical))

	return hex.EncodeToString(sum[:])
}

// postureIdentity is the complete identity of a posture theme named only by its
// key.
//
// The accepted-exception set is built from control keys, not from derived
// themes, so it needs the identity of a theme it never constructs. Doing that
// inline as theme{Kind: ..., Key: ...}.Identity() silently depends on Identity
// reading NO OTHER FIELD. That holds today, but if the canonical
// string ever gained a field, the inline literal would start producing an
// identity matching no tracked issue — every accepted close would then fall back
// to the "no longer present" wording and the completed disposition, writing the
// wrong fact into a permanent timeline, and no test would fail.
//
// Naming it keeps both producers of a posture identity in one place, so a
// change to the canonical form has one site to update rather than two.
func postureIdentity(key string) string {
	return theme{Kind: string(surfacePosture), Key: key}.Identity()
}

func postureTitle(key string) string {
	return renderTitle(theme{Kind: string(surfacePosture), Key: key})
}

// Title renders the backlog title. Stable for a given theme so a
// search-before-create can match on it.
//
// "Stable" is why no count appears here. The title carried
// pluralComponents(len(t.Components)), so a workload joining or leaving an
// otherwise unchanged theme flipped "1 workload" to "2 workloads"; the write
// half's search-before-create then missed the existing entry and re-filed the
// theme as a duplicate. That is the same churn Fingerprint deliberately avoids
// by excluding the count, reached through the other identity the write half
// matches on — so both have to exclude it, or excluding it from one accomplishes
// nothing.
//
// The count is not lost. report() renders total= and components= as their own
// fields, which is where a changing number belongs: data about the theme, not
// part of what names it.
func (t theme) Title() string {
	switch t.Kind {
	case string(surfacePosture):
		return fmt.Sprintf("security(posture): %s fails", t.Key)
	default:
		return fmt.Sprintf("security(cve): %s-severity findings", t.Key)
	}
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
// The NAME is read the same way, and for the same reason. It previously fell
// back to the summary CR's own metadata.name, which is the identical
// fabrication one field over: the CR is the scanner's object, not the workload,
// so the substituted value is a different thing that merely looks populated.
// Worse than useless — it defeats the identity guards on BOTH derivation paths,
// because each inspects the assembled component, so a summary that lost its
// label passes as identified and a broad generated designator (`kind: ".*"`)
// can suppress its finding against an identity never established.
//
// Measured over the live cluster: 2215/2215 posture and 117/117 CVE summaries
// carry the label, and in EVERY one of those 2332 the label DIFFERS from
// metadata.name. So the fallback never once fired legitimately, and wherever it
// would fire it could only substitute a wrong name. An absent label now yields
// an empty name, which the identity guards reject.
func (i item) component() component {
	return component{
		Namespace: i.Metadata.Labels["kubescape.io/workload-namespace"],
		Kind:      i.Metadata.Labels["kubescape.io/workload-kind"],
		Name:      i.Metadata.Labels["kubescape.io/workload-name"],
	}
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
//
// It decides the surface by inspecting OBJECTS, so an input carrying none gave
// it nothing to judge and it returned nil — present but inert. The command then
// reported the surface it was ASKED for as clean, on a document that never
// identified itself as belonging to it: an empty CVE list passed to -posture
// exited 0 with "nothing to file". That is the same false all-clear as
// `items: null` and the `items`+`spec` hybrid, reached where the guard is
// vacuous rather than absent.
//
// The list's top-level `kind` cannot establish it either: `kubectl get <crd>
// -o json` emits a GENERIC `apiVersion: v1, kind: List` for BOTH surfaces
// (measured against prod 2026-08-01 — 2215 posture and 359 CVE objects, `kind:
// List` in every case, for both `-A` and a single namespace), so a genuine list
// carries no surface marker at all. Refusing an input whose surface cannot be
// established is therefore the only fail-closed reading, and it costs the
// operator nothing this command documents: its usage is per-object GETs, which
// are never empty.
func checkSurface(items []item, want surface) error {
	if len(items) == 0 {
		return fmt.Errorf("%w: it carries no objects, so it cannot be shown to belong to the %s "+
			"surface; an empty list carries no surface marker (kubectl emits a generic "+
			"`kind: List` for both), so accepting it would report %s as clean on evidence "+
			"that never mentioned it. Pass per-object "+
			"`kubectl get <crd> <name> -n <ns> -o json` output, or drop the -%s flag",
			errSurfaceNotEstablished, want, want, want)
	}

	for _, it := range items {
		// The two surfaces are disjoint by key in genuine scanner output, but
		// nothing upstream enforces that. surface() returns on the first key it
		// finds, so a document carrying BOTH would be classified posture and its
		// CVE half silently ignored — and a CVE summary that also carried an empty
		// `controls: {}` would pass -posture, clear the wrong-surface guard, and
		// report an all-clear from the empty control map.
		//
		// Refusing the ambiguity is the fail-closed reading: a document that
		// identifies as both surfaces is malformed, not a posture document.
		if it.Spec.Controls != nil && it.Spec.VulnerabilitiesRef != nil {
			return fmt.Errorf("%w: %s carries both `spec.controls` and `spec.vulnerabilitiesRef`, "+
				"so which scan surface it belongs to is not decidable; genuine Kubescape output "+
				"carries exactly one",
				errMalformedScanContent, it.component())
		}

		got, ok := it.surface()
		if !ok {
			return fmt.Errorf("%w: %s carries neither `spec.controls` nor `spec.vulnerabilitiesRef`",
				errMalformedScanContent, it.component())
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
//
// The suppressed count is returned, not discarded: an all-clear reached by
// FILTERING is a different statement from one reached by a clean input, and
// report() cannot tell them apart without it.
//
// acceptedKeys names the controls whose EVERY failing component was suppressed,
// so no theme survives for them. That set is what tells the write path apart
// from a genuine disappearance: both look like "absent from themes", but one is
// remediation and the other is an accepted risk that is still failing. A control
// suppressed on one workload and live on another is NOT in it — its theme
// survives, carrying the components that remain actionable.
func derivePosture(items []item, exceptions []exception) ([]theme, int, []string, error) {
	byControl := map[string]*acc{}

	suppressed := 0
	suppressedKeys := map[string]struct{}{}

	for _, it := range items {
		ctrls, err := it.controls()
		if err != nil {
			return nil, 0, nil, err
		}

		comp := it.component()

		// Exception designators are matched against this identity, and a
		// generated cluster-wide designator uses `kind: ".*"`, which full-matches
		// the EMPTY string. So a summary missing `kubescape.io/workload-kind` —
		// malformed input, or an upstream label rename — would have its failed
		// controls suppressed by a broad exception while its identity was never
		// established, and the run would report clean.
		//
		// Establishing identity before applying any exception is the fail-closed
		// order. Namespace is deliberately not required: a cluster-scoped object
		// legitimately has none, and it is not what a broad designator turns on.
		//
		// TRIMMED, because whitespace is the same hole one step over: `" "` is
		// not equal to "" yet `.*` full-matches it just the same, so an untrimmed
		// check would establish an identity that is blank in every way that
		// matters and then let a broad exception suppress a failed control.
		if strings.TrimSpace(comp.Kind) == "" || strings.TrimSpace(comp.Name) == "" {
			return nil, 0, nil, fmt.Errorf("%w: a posture summary carries no workload %s "+
				"(`kubescape.io/workload-kind` / `kubescape.io/workload-name`); exceptions are matched "+
				"against that identity and a cluster-wide designator matches an empty value, so its "+
				"findings could be suppressed without ever identifying the resource",
				errMalformedScanContent, missingIdentityFields(comp))
		}

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
				return nil, 0, nil, fmt.Errorf("%w: %s reports control %s with status %q, "+
					"which is neither a failure nor a recognised non-failure",
					errMalformedScanContent, comp, id, ctrl.Status.Status)
			}

			if !failed {
				continue
			}

			// Severity is a failed control's prioritisation, and it is the one
			// field a theme is ordered and triaged by. JSON decoding turns a
			// missing, null or empty `severity.severity` into "", which the
			// accumulator would carry straight through to a rendered
			// `severity=` — malformed scanner output presented as a valid
			// report. Unknown NONEMPTY names stay tolerated, so a future
			// severity does not become a hard failure.
			if strings.TrimSpace(ctrl.Severity.Severity) == "" {
				return nil, 0, nil, fmt.Errorf("%w: %s reports failed control %s with no severity; "+
					"a genuine posture summary always carries one, and a theme without it "+
					"loses the prioritisation the backlog is ordered by",
					errMalformedScanContent, comp, id)
			}

			// An empty embedded ID falls back to the map key. But when BOTH are
			// present and disagree, silently preferring the embedded one applies
			// exceptions to a different control than the one keyed — a failed
			// C-9999 carrying controlID C-0036 would be suppressed by a declared
			// C-0036 exception.
			key := id
			if ctrl.ControlID != "" {
				if ctrl.ControlID != id {
					return nil, 0, nil, fmt.Errorf("%w: %s keys a control as %q but its controlID is %q; "+
						"refusing to guess which control an exception should match",
						errMalformedScanContent, comp, id, ctrl.ControlID)
				}

				key = ctrl.ControlID
			}

			// With both the map key and the embedded controlID empty the
			// fallback leaves key empty, and the theme renders as
			// "security(posture):  fails" with a stable fingerprint for a
			// control that names nothing.
			if strings.TrimSpace(key) == "" {
				return nil, 0, nil, fmt.Errorf("%w: %s reports a failed control with an empty identifier; "+
					"a backlog entry keyed on it would name no control and could not be matched "+
					"against an exception", errMalformedScanContent, comp)
			}

			// An accepted control is not actionable work. Filtering is
			// per-COMPONENT, never per-control: a kind/name-scoped exception
			// must not suppress the same control on a workload it does not
			// name, which would itself be a false all-clear.
			if excepted(exceptions, key, comp) {
				suppressed++
				suppressedKeys[key] = struct{}{}

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

	// A control with a surviving accumulator is still live somewhere, so it is
	// not accepted however many of its components were suppressed. Subtracting
	// here rather than tracking it inline keeps the loop's only new work a map
	// insert, and means the answer cannot depend on which component came first.
	accepted := make([]string, 0, len(suppressedKeys))

	for key := range suppressedKeys {
		if _, live := byControl[key]; live {
			continue
		}

		accepted = append(accepted, key)
	}

	sort.Strings(accepted)

	return assemble(surfacePosture, byControl), suppressed, accepted, nil
}

// deriveCVE groups CVE counts into one theme per severity class.
func deriveCVE(items []item) ([]theme, error) {
	bySeverity := map[string]*acc{}

	for _, it := range items {
		comp := it.component()

		// The posture path rejects a summary with no workload kind/name because
		// a cluster-wide exception designator matches an empty value. The same
		// identity feeds CVE entries and the same suppression applies, so the
		// check belongs on both paths or it covers only one of two — including
		// the TRIM: an untrimmed check here would emit a valid-looking theme for
		// an unidentified component (`components=app/ /api`), and several blank
		// identities would collapse into one another in the component set.
		if strings.TrimSpace(comp.Kind) == "" || strings.TrimSpace(comp.Name) == "" {
			return nil, fmt.Errorf("%w: a CVE summary carries no workload %s "+
				"(`kubescape.io/workload-kind` / `kubescape.io/workload-name`); exceptions are "+
				"matched against that identity and a cluster-wide designator matches an empty "+
				"value, so its findings could be suppressed without ever identifying the resource",
				errMalformedScanContent, missingIdentityFields(comp))
		}

		// A real per-object CVE response always carries its severity buckets —
		// all six of them, zero-valued when the image is clean (measured live).
		// An absent or empty map is therefore malformed input, not a clean
		// result, and iterating it zero times would report exactly that.
		if missing := missingSeverityBuckets(it.Spec.Severities); len(missing) > 0 {
			return nil, fmt.Errorf("%w: %s is missing severity bucket(s) %s; a genuine CVE summary "+
				"reports all of them, zero-valued when the image is clean, so a truncated set "+
				"could hide findings in the omitted buckets",
				errMalformedScanContent, comp, strings.Join(missing, ", "))
		}

		for class, raw := range it.Spec.Severities {
			// Extra buckets are tolerated on purpose so an upstream addition
			// does not break the run, and that tolerance is what lets a BLANK
			// key through: the six required buckets can all be present beside
			// one whose name is empty or whitespace. With a positive count it
			// became a theme rendered `security(cve): -severity findings`,
			// carrying a stable fingerprint — so it would be filed as real work
			// and then faithfully kept up to date, naming no severity at all.
			//
			// Tolerating an unknown name is right; tolerating no name is not.
			// Measured: all 117 live CVE summaries carry exactly the six
			// canonical keys and none blank, so this cannot fire on real input.
			if strings.TrimSpace(class) == "" {
				return nil, fmt.Errorf("%w: %s reports a severity bucket whose name is blank; "+
					"a genuine CVE summary names every class it counts, and a blank one would be "+
					"filed as a finding that identifies no severity",
					errMalformedScanContent, comp)
			}

			n, ok := severityCount(raw)
			if !ok {
				return nil, fmt.Errorf("%w: %s reports severity %q in an unrecognised shape; "+
					"expected an integer or {\"all\": N}", errMalformedScanContent, comp, class)
			}

			if n < 0 {
				return nil, fmt.Errorf("%w: %s reports severity %q as %d; a count cannot be negative",
					errMalformedScanContent, comp, class, n)
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
			//
			// Each bucket is validated nonnegative, but the AGGREGATE is not:
			// two valid counts can sum past MaxInt and wrap to a NEGATIVE
			// total, which then renders as `total=-N` on a successful exit. The
			// per-bucket guard cannot see it, because it fires before any
			// addition happens.
			if a.total > math.MaxInt-n {
				return nil, fmt.Errorf("%w: %s pushes the %q total past the representable maximum; "+
					"summing it would wrap to a negative count and be reported as a successful result",
					errMalformedScanContent, comp, class)
			}

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

	mode := fs.String("mode", "report", "report (default, prints what it would file) or write (reconciles issues)")
	exceptionsPath := fs.String("exceptions", "",
		"path to the generated Kubescape exceptions JSON "+
			"(`go run ./scripts/generate-kubescape-exceptions`); accepted posture controls are not backlog work")
	repo := fs.String("repo", "", "owner/name to reconcile issues in; required by -mode=write")
	inputsComplete := fs.Bool("inputs-complete", false,
		"assert that EVERY object on each supplied surface was passed. Only then may -mode=write "+
			"close a tracked issue: this command has no cluster inventory, so without the assertion "+
			"a partial run would mark live findings resolved")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Go stops flag parsing at the first non-flag argument, so
	// `-posture a.json b.json -cve c.json` silently drops b.json AND the -cve
	// flag after it, then exits 0 having read only a.json — a partial report
	// from a plausible cluster-wide invocation. Reproduced on live data.
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q: each input needs its own flag "+
			"(-posture a.json -posture b.json), because flag parsing stops at the first "+
			"non-flag argument and would silently ignore everything after it",
			errInvalidInvocation, fs.Arg(0))
	}

	if *mode != "report" && *mode != "write" {
		return fmt.Errorf("%w: unknown -mode %q (want report or write)", errInvalidInvocation, *mode)
	}

	if *mode == "write" && strings.TrimSpace(*repo) == "" {
		return fmt.Errorf("%w: -mode=write needs -repo owner/name", errWritesNotEnabled)
	}

	// -inputs-complete only ever relaxes the CLOSE gate, so a report run that
	// carries it is asserting something the run will never act on. Accepting it
	// silently would teach an operator the flag is harmless to leave set, which
	// is exactly the habit that later turns a partial write run destructive.
	if *mode != "write" && *inputsComplete {
		return fmt.Errorf("%w: -inputs-complete only affects -mode=write; "+
			"it gates closing a tracked issue", errInvalidInvocation)
	}

	// A posture run derived WITHOUT the declared exceptions keeps every accepted
	// control, and report() says so in a note because that output is explicitly
	// not a filed-work list. Write mode has no such escape: it turns the same
	// unfiltered derivation into real issues, filing — and reopening — backlog
	// work for risks the cluster has already accepted, under this command's own
	// automation disclosure. Refusing is the only reading that keeps the two
	// modes consistent, and the flag is one argument away.
	if *mode == "write" && len(posturePaths) > 0 && strings.TrimSpace(*exceptionsPath) == "" {
		return fmt.Errorf("%w: -mode=write with -posture requires -exceptions "+
			"(`go run ./scripts/generate-kubescape-exceptions`); without it every accepted "+
			"control is filed as backlog work", errWritesNotEnabled)
	}

	if len(posturePaths) == 0 && len(cvePaths) == 0 {
		return fmt.Errorf("%w: no input: pass -posture and/or -cve. "+
			"Reporting \"nothing to file\" for an empty invocation would be the same "+
			"false all-clear the stripped-LIST guard exists to prevent", errInvalidInvocation)
	}

	exceptions, err := loadExceptions(*exceptionsPath)
	if err != nil {
		return err
	}

	var (
		themes   []theme
		examined []surface
		// suppressed counts findings the declared exceptions removed. An
		// all-clear reached by FILTERING is a different claim from one reached
		// by a clean input, and the report must not render them identically.
		suppressed int
		// accepted holds the complete identities of themes an exception
		// suppressed ENTIRELY, which the planner needs both to verify the
		// historical lookup and to close them as accepted rather than as gone.
		accepted = map[string]string{}
	)

	if len(posturePaths) > 0 {
		postureItems, err := readSurface(posturePaths, surfacePosture)
		if err != nil {
			return err
		}

		derived, n, acceptedKeys, err := derivePosture(postureItems, exceptions)
		if err != nil {
			return err
		}

		suppressed = n

		// Carried as complete identities, not keys: the planner derives the
		// historical fingerprint for lookup and retains the rest to verify that
		// an accepted theme did not collide with a different tracked entry.
		for _, key := range acceptedKeys {
			accepted[postureIdentity(key)] = postureTitle(key)
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

	if *mode == "write" {
		return reconcile(themes, examined, *inputsComplete, accepted, newGHStore(*repo), out)
	}

	return report(themes, examined, len(exceptions) > 0, suppressed, out)
}

// reconcile lists what this command already tracks, plans the difference, and
// applies it. Split out of run so the write path is exercisable against a fake
// store — including the case that matters most, where the plan is empty and the
// correct behaviour is to touch nothing.
func reconcile(
	themes []theme,
	examined []surface,
	inputsComplete bool,
	accepted map[string]string,
	store issueStore,
	out io.Writer,
) error {
	existing, err := store.list()
	if err != nil {
		return err
	}

	p, err := planWrites(themes, existing, examined, inputsComplete, accepted)
	if err != nil {
		return err
	}

	if err := dropRacedCreates(&p, store); err != nil {
		return err
	}

	return applyPlan(p, store, out)
}

// dropRacedCreates re-reads the tracked set immediately before applying and
// discards any create whose complete identity has been filed since the plan was
// made. A matching fingerprint with a different identity is a collision, not a
// duplicate.
//
// Two write invocations that overlap both take the same snapshot at the top of
// reconcile, both find a newly derived theme untracked, and both plan a create.
// GitHub enforces no uniqueness on the embedded fingerprint, so both issues are
// filed — and planWrites is deliberately fail-closed on a duplicate fingerprint
// (errAmbiguousEntry, which refuses rather than guessing which of the two is
// authoritative). So a single raced create does not cost one redundant issue: it
// stops the reconciler planning ANYTHING, on every subsequent run, until an
// operator deletes one by hand.
//
// This narrows that window from the whole run — list, derive, plan, then one
// call per action — to the gap between this re-read and the create itself. It
// does NOT make the command safe to run concurrently and is not offered as a
// substitute for serializing it; a caller that schedules write mode must still
// use a `concurrency` group with cancel-in-progress disabled. doc.go records
// that as a requirement of the write path, because no scheduled caller exists
// yet to carry it.
//
// Costs one extra list per run, and only on a run that actually creates.
func dropRacedCreates(p *plan, store issueStore) error {
	if !slices.ContainsFunc(p.Actions, func(a issueAction) bool { return a.Kind == "create" }) {
		return nil
	}

	current, err := store.list()
	if err != nil {
		return err
	}

	type filedEntry struct {
		backlogEntry
		identity string
	}

	filed := map[string]filedEntry{}

	for _, e := range current {
		fp, err := e.fingerprint()
		if err != nil {
			// An unreadable marker is planWrites' business, not this filter's.
			// Refusing here would put a second, differently-worded gate on a
			// condition the plan this run is applying has already passed.
			continue
		}

		identity, _, err := e.identity()
		if err != nil {
			return err
		}
		if previous, duplicate := filed[fp]; duplicate {
			return fmt.Errorf("%w: issues #%d and #%d carry %s",
				errAmbiguousEntry, previous.Number, e.Number, fp)
		}

		filed[fp] = filedEntry{backlogEntry: e, identity: identity}
	}

	kept := make([]issueAction, 0, len(p.Actions))

	for _, a := range p.Actions {
		if a.Kind == "create" {
			// The planned body is the only place a create's identity exists —
			// it carries no issue number yet — and it is the same marker
			// backlogEntry.fingerprint parses back off a filed issue.
			planned := backlogEntry{Title: a.Title, Body: a.Body}
			fp, err := planned.fingerprint()
			if err != nil {
				return err
			}
			plannedIdentity, legacy, err := planned.identity()
			if err != nil {
				return err
			}
			if legacy {
				return fmt.Errorf("%w: planned create %q has no identity marker", errMalformedIdentity, a.Title)
			}

			if raced, exists := filed[fp]; exists {
				if raced.identity != plannedIdentity {
					return fmt.Errorf(
						"%w: issue #%d %q carries %q; planned create %q carries %q; fingerprint %s",
						errCollidingEntryIdentity, raced.Number, raced.Title, raced.identity,
						a.Title, plannedIdentity, fp)
				}
				p.RacedCreates++

				continue
			}
		}

		kept = append(kept, a)
	}

	p.Actions = kept

	return checkCreateBudget(p, len(current))
}

// checkCreateBudget refuses a plan whose creates would carry the tracked set
// past the listing budget.
//
// list() refuses a listing that EXCEEDS the budget, which protects the reader
// but not the writer: the run that crosses the line lists a set that is still
// within budget, plans its create, and files it perfectly successfully. Every
// LATER run is the one that fails, on a listing the earlier run produced. So
// without this the bridge wedges itself by working, the failure surfaces in a
// run that did nothing wrong, and recovery needs an operator stripping labels
// by hand.
//
// Checked against the re-read count, not the planning snapshot, so a create
// filed by a concurrent run in between counts against the budget too.
//
// The budget counts closed issues, so it is a lifetime total that ordinary
// successful use walks into rather than a pathological case.
func checkCreateBudget(p *plan, tracked int) error {
	creates := 0

	for _, a := range p.Actions {
		if a.Kind == "create" {
			creates++
		}
	}

	if creates == 0 {
		return nil
	}

	if projected := tracked + creates; projected > listLimit {
		return fmt.Errorf("%w: %d tracked issues plus %d new theme(s) would reach %d, over the "+
			"%d the listing can reconcile, and every later run would then refuse. Nothing was "+
			"written. To make room, remove the %q label from issues that are closed and no "+
			"longer need tracking (they are re-filed only if the finding returns), or raise "+
			"listLimit",
			errWriteFailed, tracked, creates, projected, listLimit, bridgeLabel)
	}

	return nil
}

// readSurface reads every file for one surface, validating each against that
// surface and against the stripped-skeleton signature before combining them.
//
// The same object must not arrive twice. Aggregation dedupes the component SET
// but SUMS the severity counts, so one object supplied twice — the same path
// repeated, or one object present in two files — renders a doubled total beside
// an unchanged component list: the report contradicts itself and overstates the
// platform's exposure. Since a total is what drives the backlog entry, guessing
// which of the two readings was meant is worse than refusing.
//
// Identity is the object's namespace/name, not the path. Two DIFFERENT objects
// that resolve to the same workload are the normal CVE shape — one summary per
// image — and must still aggregate, so deduping on the workload would suppress
// real findings.
func readSurface(paths []string, want surface) ([]item, error) {
	var all []item

	seen := map[string]string{}

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

		for _, it := range items {
			id := it.Metadata.Namespace + "/" + it.Metadata.Name
			if first, dup := seen[id]; dup {
				// Naming the same path twice is the likelier operator slip, and
				// "appears in both X and X" reads as a bug in the tool rather
				// than as a repeated argument.
				where := fmt.Sprintf("in both %s and %s", first, path)
				if first == path {
					where = fmt.Sprintf("twice in %s (passed more than once?)", path)
				}

				return nil, fmt.Errorf("%w: %s object %s appears %s; "+
					"severity counts are summed, so counting it twice would overstate the finding",
					errDuplicateObject, want, id, where)
			}

			seen[id] = path
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

	// The same ambiguity the exceptions loader refuses applies to SCAN input,
	// and here it hides findings rather than widening an exception: a document
	// repeating `spec.controls` decodes to the LAST value, so a real
	// failed-control map followed by `"controls":{}` exits 0 with "no live-only
	// findings". Labels, statuses and CVE counts are reachable the same way.
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %w; a repeated key here can HIDE findings — a real control "+
			"map followed by an empty one reads as a clean result", errUnrecognisedDocument, path, err)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	_, hasItems := probe["items"]
	if _, hasSpec := probe["spec"]; hasItems && hasSpec {
		// The items branch returns first, so a document carrying both would be
		// read as a list and its `spec` never examined. An empty or partial
		// `items` beside a real `spec` then reports a clean cluster — the same
		// false all-clear as `items: null`, reached by a different shape.
		return nil, fmt.Errorf("%w: %s carries both `items` and a single-object `spec`, "+
			"so whether it is a list or one object is not decidable; pass either "+
			"`kubectl get <crd> -o json` list output or one object, not a hybrid",
			errUnrecognisedDocument, path)
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
func report(themes []theme, examined []surface, filtered bool, suppressed int, out io.Writer) error {
	// A posture report derived WITHOUT the declared exceptions lists controls the
	// platform has already accepted. That is a legitimate view to ask for, but it
	// must not be mistaken for a drainable backlog — so the report says which one
	// it is rather than leaving the reader to infer it from the command line.
	//
	// It is conditioned on a posture theme actually following it. Without that
	// the note contradicts the very next line — a clean posture input printed
	// "accepted controls are included below" directly above "nothing to file" —
	// and on a run whose only themes are CVE ones it describes entries it does
	// not apply to, since exceptions are never evaluated for the CVE surface.
	postureThemes := 0

	for _, t := range themes {
		if t.Kind == string(surfacePosture) {
			postureThemes++
		}
	}

	if !filtered && slices.Contains(examined, surfacePosture) && postureThemes > 0 {
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

		// An all-clear reached by FILTERING is not the same statement as one
		// reached by a clean input, and rendering them identically is this
		// command's own core failure mode turned inward: it exists because a
		// broken scanner and a compliant cluster read alike. Saying "no
		// live-only findings" when findings were present and merely accepted is
		// literally false about the input, and it is the reading that would let
		// an exception policy widen until it accepted everything with the
		// report never changing.
		if suppressed > 0 {
			_, err := fmt.Fprintf(out, "no UNEXCEPTED live-only findings in the supplied %s input(s) — "+
				"nothing to file (%d finding(s) were present and excepted by the declared "+
				"ClusterSecurityExceptions; only the objects passed on the command line were "+
				"examined, so this is not a statement about the whole cluster)\n",
				strings.Join(names, " and "), suppressed)

			return err
		}

		_, err := fmt.Fprintf(out, "no live-only findings in the supplied %s input(s) — nothing to file "+
			"(only the objects passed on the command line were examined; "+
			"this is not a statement about the whole cluster)\n", strings.Join(names, " and "))

		return err
	}

	// The all-clear above states whether it was reached by filtering. The MIXED
	// case needs the same disclosure and is the one that actually occurs: on the
	// live cluster the declared exceptions suppress most of what fails, so a
	// reader sees a short list with no way to tell whether it is short because
	// the cluster is healthy or because the policies are broad.
	//
	// Rendering those two identically is this command's own failure mode turned
	// inward — the same reading, applied to the same numbers, that the all-clear
	// branch already refuses. Without it an exception policy can widen until it
	// accepts almost everything and the output never changes shape.
	//
	// Keyed on findings ACTUALLY suppressed, never on -exceptions having been
	// supplied: a run whose policies match nothing has filtered nothing, and
	// saying otherwise is undiscriminating in the opposite direction.
	//
	// Scoped to POSTURE, because that is the only surface exceptions reach:
	// derivePosture evaluates them and deriveCVE never sees them. Calling the
	// whole list below the unexcepted remainder would credit CVE entries with a
	// filtering that never happened, so when both surfaces are present the note
	// says so outright rather than leaving the reader to assume symmetry.
	if suppressed > 0 {
		scope := ""
		if slices.Contains(examined, surfaceCVE) {
			scope = " (CVE entries are not filtered by exception policy)"
		}

		if _, err := fmt.Fprintf(out,
			"note: %d posture finding(s) were present and excepted by the declared "+
				"ClusterSecurityExceptions; the posture entries below are the unexcepted remainder%s\n",
			suppressed, scope); err != nil {
			return err
		}
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

// missingIdentityFields names which identity labels a summary lacks, so the
// error points at the field to fix rather than at the concept.
func missingIdentityFields(c component) string {
	var missing []string
	if strings.TrimSpace(c.Kind) == "" {
		missing = append(missing, "kind")
	}

	if strings.TrimSpace(c.Name) == "" {
		missing = append(missing, "name")
	}

	return strings.Join(missing, " and ")
}
