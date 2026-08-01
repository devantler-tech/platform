package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The fixtures below use the exact shape `scripts/generate-kubescape-exceptions`
// emits — anchored regexes for both the control IDs and the resource
// attributes — so a change to the generator's output surfaces here.

func exceptionsDoc(policies ...string) string {
	return "[" + strings.Join(policies, ",") + "]"
}

// policyDoc builds one postureExceptionPolicy. Attributes are given as a raw
// JSON object so a fixture can express any designator shape.
func policyDoc(name string, controlIDs []string, resourceAttrs ...string) string {
	controls := make([]string, 0, len(controlIDs))
	for _, id := range controlIDs {
		controls = append(controls, fmt.Sprintf(`{"controlID":"^%s$"}`, id))
	}

	resources := make([]string, 0, len(resourceAttrs))
	for _, attrs := range resourceAttrs {
		resources = append(resources, fmt.Sprintf(`{"designatorType":"Attributes","attributes":%s}`, attrs))
	}

	return fmt.Sprintf(`{"name":%q,"policyType":"postureExceptionPolicy","actions":["alertOnly"],`+
		`"resources":[%s],"posturePolicies":[%s],"reason":"test fixture"}`,
		name, strings.Join(resources, ","), strings.Join(controls, ","))
}

func loadFixture(t *testing.T, raw string) []exception {
	t.Helper()

	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, raw)

	got, err := loadExceptions(path)
	if err != nil {
		t.Fatalf("loadExceptions: %v", err)
	}

	return got
}

// A control the platform has deliberately accepted is not actionable backlog
// work. Without this filter the bridge queues every matching exception as a new
// issue, recreating exactly the noise the exception pipeline removes.
func TestClusterWideExceptionSuppressesAcceptedControl(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(policyDoc("admission-controllers", []string{"C-0036"}, `{"kind":".*"}`)))

	items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0036": "failed"}))

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 0 {
		t.Errorf("an accepted control must not become backlog work, got %+v", got)
	}
}

// THE control that makes the test above meaningful. Filtering per-CONTROL
// instead of per-COMPONENT would suppress this finding too — turning an
// exception scoped to Jobs into a cluster-wide silence, which is itself the
// false all-clear class this command exists to prevent.
func TestScopedExceptionDoesNotSuppressAWorkloadItDoesNotName(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(policyDoc("batch-workloads", []string{"C-0016"}, `{"kind":"^Job$"}`)))

	items := itemsOf(t,
		postureDoc("app", "Job", "nightly", map[string]string{"C-0016": "failed"}),
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
	)

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 1 {
		t.Fatalf("the Deployment finding must survive, got %d themes: %+v", len(got), got)
	}

	if got[0].Components[0] != "app/Deployment/api" {
		t.Errorf("want only the un-excepted Deployment, got %v", got[0].Components)
	}

	for _, c := range got[0].Components {
		if strings.Contains(c, "Job") {
			t.Errorf("the excepted Job must be filtered out, got %v", got[0].Components)
		}
	}
}

// A kind+name designator must match BOTH fields, so a same-kind workload with a
// different name keeps its finding.
func TestNameScopedExceptionMatchesOnlyThatName(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(
		policyDoc("one-workload", []string{"C-0016"}, `{"kind":"^Deployment$","name":"^api$"}`)))

	items := itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureDoc("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
	)

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 1 || len(got[0].Components) != 1 || got[0].Components[0] != "app/Deployment/web" {
		t.Errorf("only the named workload may be excepted, got %+v", got)
	}
}

// An exception for a DIFFERENT control must not suppress this one.
func TestExceptionForAnotherControlDoesNotSuppress(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(policyDoc("other", []string{"C-0999"}, `{"kind":".*"}`)))

	items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 1 {
		t.Errorf("an unrelated exception must not suppress a real finding, got %+v", got)
	}
}

// The control IDs are anchored regexes in the generated document, so a prefix
// must not match a longer ID.
func TestAnchoredControlIDDoesNotMatchByPrefix(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(policyDoc("prefix", []string{"C-001"}, `{"kind":".*"}`)))

	items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 1 {
		t.Errorf("^C-001$ must not except C-0016, got %+v", got)
	}
}

// TestUnanchoredControlIDIsRejected closes the gap every other fixture here hides. policyDoc
// anchors each controlID with ^…$, so the whole suite proves that an ANCHORED pattern matches
// exactly — never that an unanchored one is refused. It is not, and covers() uses MatchString, so
// a raw "C-001" substring-matches C-0016 and silently suppresses a control the platform never
// accepted. That widens what the cluster is treated as having accepted, which is the one direction
// an exception loader must never be wrong in.
//
// The generator is already the authority on this: its anchor() returns fully anchored values and
// rejects partial anchoring for exactly this reason. The loader enforces the same contract rather
// than trusting that every artifact reaching it was generated.
func TestUnanchoredControlIDIsRejected(t *testing.T) {
	raw := `[{"name":"unanchored","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],` +
		`"posturePolicies":[{"controlID":"C-001"}],"reason":"test fixture"}]`

	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, raw)

	if _, err := loadExceptions(path); err == nil {
		t.Fatal("an unanchored controlID substring-matches other controls and must be rejected")
	}
}

// TestPartiallyAnchoredControlIDIsRejected is the other half of the generator's contract: a value
// anchored at only one end is still substring-matchable at the open end, so "^C-001" matches
// C-0016 just as the bare form does.
func TestPartiallyAnchoredControlIDIsRejected(t *testing.T) {
	raw := `[{"name":"partial","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],` +
		`"posturePolicies":[{"controlID":"^C-001"}],"reason":"test fixture"}]`

	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, raw)

	if _, err := loadExceptions(path); err == nil {
		t.Fatal("a partially anchored controlID is still substring-matchable and must be rejected")
	}
}

// TestAlternationCannotDefeatAnchoring covers the hole a TEXTUAL anchoring check cannot see. In a
// regex, `^` and `$` bind to the individual alternation branch they sit in, so `^C-001|C-002$` is
// `(^C-001)|(C-002$)`: it starts with ^ and ends with $, satisfying any prefix/suffix test, while
// branch one is open at the END (it matches C-0016) and branch two is open at the START (it matches
// XC-002). Validating anchoring as text is therefore not enough on its own — the compiled pattern
// has to carry full-match semantics.
func TestAlternationCannotDefeatAnchoring(t *testing.T) {
	raw := `[{"name":"alternation","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],` +
		`"posturePolicies":[{"controlID":"^C-001|C-002$"}],"reason":"test fixture"}]`

	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, raw)

	got, err := loadExceptions(path)
	if err != nil {
		return // rejecting the value outright is an equally correct answer
	}

	// Accepted, so the compiled pattern must not reach beyond the two branches it names.
	items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))

	if derived := mustDerivePosture(t, items, got); len(derived) != 1 {
		t.Errorf("^C-001|C-002$ must not except C-0016, got %+v", derived)
	}

	other := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"XC-002": "failed"}))

	if derived := mustDerivePosture(t, other, got); len(derived) != 1 {
		t.Errorf("^C-001|C-002$ must not except XC-002, got %+v", derived)
	}
}

// TestUnanchoredAttributeDoesNotMatchBySubstring is the sibling path the control-ID finding only
// named one of. Resource attributes are compiled and matched with the same MatchString, so a
// `kind: Job` designator substring-matched CronJob and silently widened the exception to a workload
// kind the platform never accepted.
func TestUnanchoredAttributeDoesNotMatchBySubstring(t *testing.T) {
	raw := `[{"name":"substring-kind","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":"Job"}}],` +
		`"posturePolicies":[{"controlID":"^C-0016$"}],"reason":"test fixture"}]`

	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, raw)

	got, err := loadExceptions(path)
	if err != nil {
		return // rejecting it is equally correct
	}

	items := itemsOf(t, postureDoc("app", "CronJob", "backup", map[string]string{"C-0016": "failed"}))

	if derived := mustDerivePosture(t, items, got); len(derived) != 1 {
		t.Errorf("a `kind: Job` designator must not except a CronJob, got %+v", derived)
	}
}

// An attribute this command does not model must NOT be treated as satisfied:
// doing so would widen the exception and hide a real finding.
func TestUnknownDesignatorAttributeDoesNotExcept(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(
		policyDoc("future", []string{"C-0016"}, `{"someFutureAttribute":".*"}`)))

	items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 1 {
		t.Errorf("an unmodelled designator attribute must not except anything, got %+v", got)
	}
}

// A policy with no designators matches nothing, so an unscoped-by-accident
// policy cannot silently suppress the whole cluster.
//
// The LOADER now refuses such a policy outright
// (TestExceptionPolicyThatCanCoverNothingIsRejected), which is strictly
// stronger than tolerating one that matches nothing. This test therefore pins
// the property one layer DOWN, at covers() itself, constructing the value
// directly instead of loading it — so if the loader guard is ever loosened, the
// matcher must still fail closed rather than the guarantee vanishing along with
// the guard that superseded it.
func TestPolicyWithNoResourcesCoversNothing(t *testing.T) {
	scopeless := exception{
		name:     "scopeless",
		controls: []*regexp.Regexp{regexp.MustCompile("^C-0016$")},
	}

	if scopeless.covers("C-0016", component{Namespace: "app", Kind: "Deployment", Name: "api"}) {
		t.Error("a policy with no resource designators must cover nothing, even for a control it names")
	}
}

// The same guarantee for the other half: designators but no controls.
func TestPolicyWithNoControlsCoversNothing(t *testing.T) {
	uncontrolled := exception{
		name:      "no-controls",
		resources: []designator{{"kind": regexp.MustCompile("^(?:.*)$")}},
	}

	if uncontrolled.covers("C-0016", component{Namespace: "app", Kind: "Deployment", Name: "api"}) {
		t.Error("a policy naming no controls must cover nothing, even for a workload it matches")
	}
}

// This test previously asserted that an unknown policyType is silently ignored,
// on the stated premise that "the generator emits others". That premise is
// false: generate-kubescape-exceptions hard-codes "postureExceptionPolicy" for
// every policy it emits, its own test pins that for every policy, and the real
// generated artifact is 21 policies of exactly that one type. Skipping was
// therefore never reading a legitimate other type — it could only ever swallow
// a stale or schema-changed artifact, quietly narrowing what counts as accepted
// so accepted controls reappear as backlog work. Rejection is covered by
// TestUnknownExceptionPolicyTypeIsRejected; this is the over-tightening control
// that the one type the generator DOES emit still loads.
func TestGeneratedPolicyTypeIsAccepted(t *testing.T) {
	raw := `[{"name":"ok","policyType":"` + posturePolicyType + `","actions":["alertOnly"],` +
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":"^Deployment$"}}],` +
		`"posturePolicies":[{"controlID":"^C-0016$"}]}]`

	exceptions := loadFixture(t, raw)
	if len(exceptions) != 1 {
		t.Fatalf("the generator's own policy type must load, got %d", len(exceptions))
	}
}

// Silently ignoring a malformed exceptions file would re-file accepted
// controls; silently widening one would hide real findings. Both are hard
// errors.
func TestMalformedExceptionsDocumentIsAHardError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, `{"not":"an array"}`)

	_, err := loadExceptions(path)
	if err == nil {
		t.Fatal("a malformed exceptions document must be rejected")
	}

	if !errors.Is(err, errBadExceptions) {
		t.Errorf("want errBadExceptions, got %v", err)
	}
}

func TestUncompilablePatternIsAHardError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, `[{"name":"bad","policyType":"postureExceptionPolicy","actions":["alertOnly"],`+
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":"("}}],`+
		`"posturePolicies":[{"controlID":"^C-0016$"}]}]`)

	_, err := loadExceptions(path)
	if err == nil {
		t.Fatal("an uncompilable designator pattern must be rejected")
	}

	if !errors.Is(err, errBadExceptions) {
		t.Errorf("want errBadExceptions, got %v", err)
	}
}

func TestMissingExceptionsFileIsAHardError(t *testing.T) {
	_, err := loadExceptions(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("a named but unreadable exceptions file must be an error, not silently skipped")
	}

	if !errors.Is(err, errBadExceptions) {
		t.Errorf("want errBadExceptions, got %v", err)
	}
}

// No -exceptions flag is a legitimate configuration: every failed control is
// reported.
func TestAbsentExceptionsFlagFiltersNothing(t *testing.T) {
	got, err := loadExceptions("")
	if err != nil {
		t.Fatalf("an empty path must be accepted, got %v", err)
	}

	if len(got) != 0 {
		t.Errorf("an empty path must yield no exceptions, got %d", len(got))
	}
}

// End-to-end through the real CLI surface: the same input reports a finding
// without -exceptions and reports nothing with it.
func TestExceptionsFlagChangesTheReportEndToEnd(t *testing.T) {
	dir := t.TempDir()
	posture := filepath.Join(dir, "posture.json")
	writeBare(t, posture, postureDoc("app", "Deployment", "api", map[string]string{"C-0036": "failed"}))

	exceptions := filepath.Join(dir, "exceptions.json")
	writeRaw(t, exceptions, exceptionsDoc(policyDoc("admission-controllers", []string{"C-0036"}, `{"kind":".*"}`)))

	var without bytes.Buffer
	if err := run([]string{"-posture", posture}, &without); err != nil {
		t.Fatalf("without exceptions: %v", err)
	}

	if !strings.Contains(without.String(), "C-0036") {
		t.Fatalf("guard: the baseline must report the control, got %q", without.String())
	}

	var with bytes.Buffer
	if err := run([]string{"-posture", posture, "-exceptions", exceptions}, &with); err != nil {
		t.Fatalf("with exceptions: %v", err)
	}

	if strings.Contains(with.String(), "C-0036") {
		t.Errorf("the accepted control must not be reported, got %q", with.String())
	}

	if !strings.Contains(with.String(), "nothing to file") {
		t.Errorf("want an explicit empty report, got %q", with.String())
	}
}

// An unknown designator type must not be compiled as an attribute matcher: it
// would suppress real controls under semantics this command does not implement.
func TestUnknownDesignatorTypeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, `[{"name":"future","policyType":"postureExceptionPolicy","actions":["alertOnly"],`+
		`"resources":[{"designatorType":"future-type","attributes":{"kind":".*"}}],`+
		`"posturePolicies":[{"controlID":"^C-0016$"}]}]`)

	_, err := loadExceptions(path)
	if err == nil {
		t.Fatal("an unknown designator type must be refused, not compiled as Attributes")
	}

	if !errors.Is(err, errBadExceptions) {
		t.Errorf("want errBadExceptions, got %v", err)
	}
}

// A policy that does not declare the generator's action must not be applied.
// The action is what makes it an exception; without it the loader would be
// suppressing findings under semantics it never checked.
func TestPolicyWithoutAlertOnlyActionIsRejected(t *testing.T) {
	for name, actions := range map[string]string{
		"absent":     ``,
		"empty":      `"actions":[],`,
		"other":      `"actions":["disable"],`,
		"extra":      `"actions":["alertOnly","disable"],`,
		"wrong case": `"actions":["ALERTONLY"],`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := `[{"name":"p","policyType":"postureExceptionPolicy",` + actions +
				`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],` +
				`"posturePolicies":[{"controlID":"^C-0016$"}]}]`

			path := filepath.Join(t.TempDir(), "exceptions.json")
			writeRaw(t, path, raw)

			if _, err := loadExceptions(path); !errors.Is(err, errBadExceptions) {
				t.Fatalf("want errBadExceptions, got %v", err)
			}
		})
	}
}

// The generator's own shape must keep loading, so the guard above cannot be
// satisfied by simply refusing everything.
func TestGeneratorActionIsAccepted(t *testing.T) {
	raw := `[{"name":"p","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],` +
		`"posturePolicies":[{"controlID":"^C-0016$"}]}]`

	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, raw)

	got, err := loadExceptions(path)
	if err != nil {
		t.Fatalf("generator shape must load: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 compiled policy, got %d", len(got))
	}
}

// A policy that compiles but can never match anything is not harmless. covers()
// already fails closed on it — no controls or no designators means it suppresses
// nothing — so the SUPPRESSION direction is safe. The damage is to the REPORT:
// run() passes `len(exceptions) > 0` as proof that filtering occurred, so one
// vacuous policy suppresses the "exceptions were NOT applied" caveat while
// accepted controls still render as ordinary backlog work. The operator is told
// the output is a filed-work list with accepted controls removed, when nothing
// was removed at all.
func TestExceptionPolicyThatCanCoverNothingIsRejected(t *testing.T) {
	for name, raw := range map[string]string{
		"no posturePolicies": `[{"name":"p","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
			`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],` +
			`"posturePolicies":[]}]`,
		"every controlID empty": `[{"name":"p","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
			`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],` +
			`"posturePolicies":[{"controlID":""}]}]`,
		"no resources": `[{"name":"p","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
			`"resources":[],"posturePolicies":[{"controlID":"^C-0016$"}]}]`,
		"designator with no attributes": `[{"name":"p","policyType":"postureExceptionPolicy","actions":["alertOnly"],` +
			`"resources":[{"designatorType":"Attributes","attributes":{}}],` +
			`"posturePolicies":[{"controlID":"^C-0016$"}]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exceptions.json")
			writeRaw(t, path, raw)

			if _, err := loadExceptions(path); !errors.Is(err, errBadExceptions) {
				t.Fatalf("want errBadExceptions, got %v", err)
			}
		})
	}
}

// The over-tightening control for the guard above: a policy that names a
// control AND a designator still loads and still suppresses. Without this, the
// guard could be satisfied by refusing every policy.
func TestPolicyThatCanCoverSomethingStillSuppresses(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(policyDoc("admission-controllers", []string{"C-0036"}, `{"kind":".*"}`)))

	items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0036": "failed"}))

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 0 {
		t.Errorf("an accepted control must still be suppressed, got %+v", got)
	}
}
