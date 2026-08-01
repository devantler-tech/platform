package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
)

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

// errBadExceptions reports an exceptions document this command cannot apply.
// It is a hard error: silently ignoring a malformed exceptions file would
// re-file accepted controls, and silently WIDENING one would hide real
// findings.
var errBadExceptions = errors.New("cannot read the generated Kubescape exceptions")

// posturePolicyType is the only policy type this command applies. The
// generator emits others for surfaces this bridge does not derive.
const posturePolicyType = "postureExceptionPolicy"

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
	Resources  []struct {
		Attributes map[string]string `json:"attributes"`
	} `json:"resources"`
	PosturePolicies []struct {
		ControlID string `json:"controlID"`
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
			continue
		}

		compiled, err := compilePolicy(p, path)
		if err != nil {
			return nil, err
		}

		out = append(out, compiled)
	}

	return out, nil
}

func compilePolicy(p rawPolicy, path string) (exception, error) {
	e := exception{name: p.Name}

	for _, pp := range p.PosturePolicies {
		if pp.ControlID == "" {
			continue
		}

		re, err := regexp.Compile(pp.ControlID)
		if err != nil {
			return exception{}, fmt.Errorf("%w: %s: policy %q: controlID %q: %w",
				errBadExceptions, path, p.Name, pp.ControlID, err)
		}

		e.controls = append(e.controls, re)
	}

	for _, r := range p.Resources {
		d := designator{}

		for key, pattern := range r.Attributes {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return exception{}, fmt.Errorf("%w: %s: policy %q: attribute %s=%q: %w",
					errBadExceptions, path, p.Name, key, pattern, err)
			}

			d[key] = re
		}

		e.resources = append(e.resources, d)
	}

	return e, nil
}
