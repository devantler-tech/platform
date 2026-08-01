// This file applies the platform's declared ClusterSecurityExceptions.
//
// A declared ClusterSecurityException records a control the platform has
// deliberately accepted — runtime-enforced elsewhere (Kyverno mutation, a
// CiliumNetworkPolicy) or irreducible. Queueing one as backlog work recreates
// exactly the noise the exception pipeline exists to remove.
//
// The CRs under k8s/bases/infrastructure/cluster-security-exceptions/ are the
// single source of truth, and scripts/generate-kubescape-exceptions already
// converts them to Kubescape's native format for the CI scan. This command
// consumes THAT artifact rather than re-parsing the CRs, so the two consumers
// cannot drift apart in what they consider accepted.
//
// In that format both the control IDs and the resource attributes are anchored
// REGEXES (`^C-0036$`, `kind: ^Job$`, `kind: .*`), which is why matching here
// compiles them rather than comparing strings.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// errBadExceptions reports an exceptions document this command cannot apply.
// It is a hard error: silently ignoring a malformed exceptions file would
// re-file accepted controls, and silently WIDENING one would hide real
// findings.
var errBadExceptions = errors.New("cannot read the generated Kubescape exceptions")

// posturePolicyType is the only policy type this command applies. The
// generator emits others for surfaces this bridge does not derive.
const posturePolicyType = "postureExceptionPolicy"

// exceptionActionAlertOnly is the only exception action this command
// understands, and the only one the generator emits.
const exceptionActionAlertOnly = "alertOnly"

// attributesDesignator is the only resource designator type this command
// implements; the generator emits no other today.
const attributesDesignator = "Attributes"

// exception is one compiled exception policy.
type exception struct {
	name string
	// controls are the anchored control-ID patterns this policy accepts.
	controls []*regexp.Regexp
	// resources are the designators it is scoped to. A policy with no
	// designators matches nothing, so an unscoped-by-accident policy cannot
	// silently suppress the whole cluster.
	resources []designator
}

// designator matches a component's structured fields. Every attribute must
// match for the designator to apply.
type designator map[string]*regexp.Regexp

// matches reports whether this designator covers the component.
//
// An UNKNOWN attribute key never matches. That is deliberate and conservative:
// an attribute this command does not model must not be treated as satisfied,
// because doing so would widen the exception and hide a real finding.
func (d designator) matches(c component) bool {
	if len(d) == 0 {
		return false
	}

	for key, re := range d {
		var value string

		switch key {
		case "kind":
			value = c.Kind
		case "name":
			value = c.Name
		case "namespace":
			value = c.Namespace
		default:
			return false
		}

		if !re.MatchString(value) {
			return false
		}
	}

	return true
}

// covers reports whether this policy accepts the given control on the given
// component. Both halves must match: the control ID AND the resource scope.
func (e exception) covers(controlID string, c component) bool {
	matchedControl := false

	for _, re := range e.controls {
		if re.MatchString(controlID) {
			matchedControl = true

			break
		}
	}

	if !matchedControl {
		return false
	}

	for _, d := range e.resources {
		if d.matches(c) {
			return true
		}
	}

	return false
}

// excepted reports whether any declared exception accepts this finding.
func excepted(exceptions []exception, controlID string, c component) bool {
	for _, e := range exceptions {
		if e.covers(controlID, c) {
			return true
		}
	}

	return false
}

// rawPolicy mirrors the generator's output shape.
type rawPolicy struct {
	Name       string `json:"name"`
	PolicyType string `json:"policyType"`
	// Actions is what the policy asks Kubescape to DO with a matched control.
	// The generator emits exactly ["alertOnly"]; discarding the field would let
	// an artifact declaring some other action still act as a full suppressor
	// here — see compilePolicy, which requires it rather than assuming it.
	Actions   []string `json:"actions"`
	Resources []struct {
		DesignatorType string            `json:"designatorType"`
		Attributes     map[string]string `json:"attributes"`
	} `json:"resources"`
	PosturePolicies []struct {
		ControlID string `json:"controlID"`
		// FrameworkName scopes an exception to one framework. The generator
		// supports it, so it must be read even though no current policy uses it
		// — see compilePolicy, which refuses rather than silently widening.
		FrameworkName string `json:"frameworkName"`
	} `json:"posturePolicies"`
}

// loadExceptions reads and compiles the generated exceptions document.
//
// An empty path means no exceptions were supplied, which is a legitimate
// configuration — the bridge then reports every failed control. Anything
// present but unreadable, unparseable, or carrying a pattern that does not
// compile is a hard error rather than a silent skip.
func loadExceptions(path string) ([]exception, error) {
	if path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied generator output path; a CLI argument by design
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", errBadExceptions, path, err)
	}

	var policies []rawPolicy
	if err := json.Unmarshal(raw, &policies); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", errBadExceptions, path, err)
	}

	out := make([]exception, 0, len(policies))

	for _, p := range policies {
		if p.PolicyType != posturePolicyType {
			// Silently skipping meant a stale or schema-changed artifact
			// quietly narrowed what counts as accepted: controls it was meant
			// to cover reappear as backlog work, and the write path would
			// re-file findings the platform has already accepted. The
			// generator emits exactly one type, so anything else means the
			// artifact and this reader disagree.
			return nil, fmt.Errorf("%w: %s declares policy %q with unknown policyType %q; "+
				"expected %q — regenerate the artifact rather than reading it partially",
				errBadExceptions, path, p.Name, p.PolicyType, posturePolicyType)
		}

		compiled, err := compilePolicy(p, path)
		if err != nil {
			return nil, err
		}

		out = append(out, compiled)
	}

	return out, nil
}

// compileFullMatch compiles a declared pattern so it can only ever match a WHOLE value.
//
// Both consumers match with MatchString, which succeeds on a substring, and validating the text for
// leading ^ and trailing $ is not sufficient on its own: those anchors bind to the individual
// alternation branch they sit in, so `^C-001|C-002$` reads as `(^C-001)|(C-002$)` — it passes a
// prefix/suffix check while branch one stays open at the end (matching C-0016) and branch two open
// at the start (matching XC-002).
//
// Wrapping in a non-capturing group makes full-match semantics a property of the compiled pattern
// rather than of its spelling, so no internal structure can widen it. Already-anchored values stay
// correct — the anchors are zero-width and still match at the same positions.
func compileFullMatch(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("^(?:" + pattern + ")$")
}

func compilePolicy(p rawPolicy, path string) (exception, error) {
	e := exception{name: p.Name}

	// The action is what makes a policy an exception at all. The generator emits
	// exactly ["alertOnly"] for every policy it writes, so anything else did not
	// come from it — a stale, hand-edited or schema-changed artifact. Applying it
	// anyway would suppress findings under semantics this command never checked,
	// which is the same silent-widening direction the anchoring rules below guard.
	//
	// Requiring it is fail-closed and cannot fire on today's artifact.
	if len(p.Actions) != 1 || p.Actions[0] != exceptionActionAlertOnly {
		return exception{}, fmt.Errorf("%w: %s: policy %q declares actions %v, but this command only "+
			"understands exactly [%q]; a policy carrying any other action would be applied as a full "+
			"suppressor under semantics that were never validated",
			errBadExceptions, path, p.Name, p.Actions, exceptionActionAlertOnly)
	}

	for _, pp := range p.PosturePolicies {
		if pp.ControlID == "" {
			continue
		}

		// A framework-scoped exception accepts a control only within one
		// framework. The scan summaries this command reads carry no framework,
		// so the scope cannot be honoured — and applying the policy anyway
		// would widen a deliberately narrow exception across every framework,
		// suppressing controls the platform never accepted.
		//
		// Refusing is the fail-closed half of that choice: it is a loud, fixable
		// error, where widening is silent. No current policy uses the field
		// (0 of 93 measured), so this cannot fire on today's artifact.
		if pp.FrameworkName != "" {
			return exception{}, fmt.Errorf("%w: %s: policy %q scopes control %q to framework %q, "+
				"but the scan input carries no framework to match it against; "+
				"applying it would widen the exception across every framework",
				errBadExceptions, path, p.Name, pp.ControlID, pp.FrameworkName)
		}

		// covers() matches with MatchString, which succeeds on a SUBSTRING. An
		// unanchored "C-001" therefore suppresses C-0016 as well — widening a
		// deliberately narrow exception onto controls the platform never
		// accepted, silently, and in the one direction this loader must never be
		// wrong in.
		//
		// The generator that produces these artifacts already anchors every value
		// and rejects partial anchoring for exactly this reason. Enforcing the
		// same contract here means a hand-edited or otherwise ungenerated artifact
		// fails loudly instead of quietly excepting more than it names. A value
		// anchored at only one end is still substring-matchable at the open end,
		// so it is refused too rather than silently completed.
		if !strings.HasPrefix(pp.ControlID, "^") || !strings.HasSuffix(pp.ControlID, "$") {
			return exception{}, fmt.Errorf("%w: %s: policy %q: controlID %q is not fully anchored; "+
				"controls are matched as substrings, so it would also suppress every control whose ID "+
				"contains it. Anchor it as ^…$",
				errBadExceptions, path, p.Name, pp.ControlID)
		}

		re, err := compileFullMatch(pp.ControlID)
		if err != nil {
			return exception{}, fmt.Errorf("%w: %s: policy %q: controlID %q: %w",
				errBadExceptions, path, p.Name, pp.ControlID, err)
		}

		e.controls = append(e.controls, re)
	}

	for _, r := range p.Resources {
		// The discriminator must be honoured, not discarded. An unknown
		// designator type whose attributes happen to parse would compile into a
		// working attribute matcher and could suppress real controls under
		// semantics this command does not implement.
		if r.DesignatorType != attributesDesignator {
			return exception{}, fmt.Errorf("%w: %s: policy %q uses designator type %q, "+
				"but this command only implements %q",
				errBadExceptions, path, p.Name, r.DesignatorType, attributesDesignator)
		}

		if len(r.Attributes) == 0 {
			return exception{}, fmt.Errorf("%w: %s: policy %q carries a resource designator with no "+
				"attributes, which names no workload and can never match; a policy that cannot "+
				"cover anything still counts as filtering applied and would suppress the "+
				"unfiltered-report caveat",
				errBadExceptions, path, p.Name)
		}

		d := designator{}

		for key, pattern := range r.Attributes {
			re, err := compileFullMatch(pattern)
			if err != nil {
				return exception{}, fmt.Errorf("%w: %s: policy %q: attribute %s=%q: %w",
					errBadExceptions, path, p.Name, key, pattern, err)
			}

			d[key] = re
		}

		e.resources = append(e.resources, d)
	}

	// A policy needs BOTH halves to cover anything: covers() requires a control
	// match AND a designator match. Missing either, it suppresses nothing — so
	// the suppression direction is already safe and this guard is not about
	// hidden findings.
	//
	// The damage is to the REPORT. run() passes `len(exceptions) > 0` as proof
	// that filtering occurred, so a single vacuous policy suppresses the
	// "declared exceptions were NOT applied" caveat while accepted controls
	// still render as ordinary backlog work. The operator is told the output is
	// a filed-work list with accepted controls removed, when nothing was
	// removed — a report that misstates its own provenance.
	//
	// Both halves are fail-closed and cannot fire on today's artifact: every one
	// of the generator's 21 policies names at least one control and one
	// designator.
	if len(e.controls) == 0 {
		return exception{}, fmt.Errorf("%w: %s: policy %q resolves to no control patterns, so it can "+
			"never cover a finding, yet it would still count as filtering applied",
			errBadExceptions, path, p.Name)
	}

	if len(e.resources) == 0 {
		return exception{}, fmt.Errorf("%w: %s: policy %q names no resource designators, so it can "+
			"never cover a finding, yet it would still count as filtering applied",
			errBadExceptions, path, p.Name)
	}

	return e, nil
}
