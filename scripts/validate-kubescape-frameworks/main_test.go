// Behaviour tests for the Kubescape framework guard.
//
// The guard protects against a regression that LOOKS LIKE AN IMPROVEMENT:
// dropping a framework removes findings, so the compliance score rises and every
// check goes green. The interesting failure mode is therefore the guard PASSING
// when it should not — so every accepted case below is paired with a rejected
// one, and the decoys are the cases a previous iteration let through.
package main

import (
	"os"
	"strings"
	"testing"
)

// runBlock wraps body lines in a minimal workflow whose only `run:` scalar is
// the body under test.
func runBlock(body string) string {
	var b strings.Builder
	b.WriteString("jobs:\n  validate:\n    steps:\n      - name: scan\n        run: |\n")
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		b.WriteString("          " + line + "\n")
	}
	return b.String()
}

// setOf parses a synthetic workflow and returns its normalised framework set,
// or an error. It exercises the same path main() uses.
func setOf(t *testing.T, body string) ([]string, error) {
	t.Helper()
	path := writeTemp(t, runBlock(body))
	return frameworkSet(path)
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/workflow.yaml"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

const goodScan = "ksail workload scan --framework nsa,mitre --compliance-threshold 95"

func TestAcceptsCurrentConfiguration(t *testing.T) {
	got, err := setOf(t, goodScan)
	if err != nil {
		t.Fatalf("expected the guard to ACCEPT the shipped configuration, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("normalised set = %q, want %q", strings.Join(got, ","), "mitre,nsa")
	}
}

// Ordering and repetition must not make two equal sets compare unequal.
func TestNormalisesOrderAndRepetition(t *testing.T) {
	a, err := setOf(t, "ksail workload scan --framework mitre,nsa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := setOf(t, "ksail workload scan --framework nsa,mitre,nsa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("order/repetition changed the set: %q vs %q", a, b)
	}
}

// A framework name carrying punctuation must survive parsing INTACT. A character
// class listing permitted characters TRUNCATES instead of failing, so
// `cis-v1.23-t1.0.1` and `cis-v1.24-t1.0.0` both became `cis` and two genuinely
// different sets compared equal.
func TestPunctuatedNameSurvivesIntact(t *testing.T) {
	got, err := setOf(t, "ksail workload scan --framework nsa,mitre,cis-v1.23-t1.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(got, ",") != "cis-v1.23-t1.0.1,mitre,nsa" {
		t.Fatalf("punctuated name was not preserved: %q", got)
	}
}

func TestRejectsMissingRequiredFramework(t *testing.T) {
	for _, body := range []string{
		"ksail workload scan --framework nsa --compliance-threshold 95",
		"ksail workload scan --framework mitre --compliance-threshold 95",
		"ksail workload scan --framework pss --compliance-threshold 95",
	} {
		set, err := setOf(t, body)
		if err != nil {
			continue // read failure is also a rejection
		}
		if err := checkRequired("ci.yaml", set); err == nil {
			t.Fatalf("expected REJECT for %q — a coverage regression would pass", body)
		}
	}
}

// A substring must not satisfy a whole element, or a future rename would pass.
func TestRejectsSubstringOfRequiredFramework(t *testing.T) {
	set, err := setOf(t, "ksail workload scan --framework nsa-extended,mitre")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := checkRequired("ci.yaml", set); err == nil {
		t.Fatal("expected REJECT: `nsa-extended` must not satisfy `nsa`")
	}
}

// A list the guard cannot read literally must FAIL CLOSED rather than be
// silently truncated to whatever prefix happens to match.
func TestRejectsVariableFrameworkList(t *testing.T) {
	if _, err := setOf(t, `ksail workload scan --framework "$FRAMEWORKS" --compliance-threshold 95`); err == nil {
		t.Fatal("expected FAIL CLOSED on a variable framework list")
	}
}

// Zero invocations must be an error. Exiting 0 there would report a protected
// repository while checking absolutely nothing.
func TestFailsClosedWhenNoInvocation(t *testing.T) {
	if _, err := setOf(t, "echo nothing to see here"); err == nil {
		t.Fatal("expected FAIL CLOSED when no scan invocation exists")
	}
}

// Two invocations are REJECTED, not unioned: the union loses which set produced
// the SARIF that actually reaches the uploader.
func TestRejectsTwoInvocations(t *testing.T) {
	body := "ksail workload scan --framework nsa,mitre,pss -o up.sarif\n" +
		"ksail workload scan --framework nsa,mitre -o throwaway.sarif"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected REJECT: two invocations hide which set is uploaded")
	}
}

// The same rule at line granularity: two scans chained on ONE line.
func TestRejectsTwoScansOnOneLine(t *testing.T) {
	body := "ksail workload scan --framework nsa,mitre,pss -o up.sarif && ksail workload scan --framework nsa,mitre -o throwaway.sarif"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected REJECT: chained scans hide which set is uploaded")
	}
}

// --- The DECOY class: output-only and non-executing forms --------------------
//
// Each of these would have needed its own exclusion under a blacklist. They are
// rejected here by requiring `ksail` as the first token of a line that is not
// inside a heredoc body.
func TestRejectsOutputOnlyDecoys(t *testing.T) {
	cases := map[string]string{
		"comment decoy": "# ksail workload scan --framework nsa,mitre\n" +
			`ksail workload scan --framework "$FRAMEWORKS"`,
		"echo decoy": `echo "ksail workload scan --framework nsa,mitre"` + "\n" +
			`ksail workload scan --framework "$FRAMEWORKS"`,
		"printf decoy": `printf 'ksail workload scan --framework nsa,mitre\n'` + "\n" +
			`ksail workload scan --framework "$FRAMEWORKS"`,
		"shell-prefix decoy": "true && ksail workload scan --framework nsa,mitre\n" +
			`ksail workload scan --framework "$FRAMEWORKS"`,
	}
	for name, body := range cases {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected REJECT — the decoy supplied the framework list the guard read", name)
		}
	}
}

// --- #3060: the shell-CONTEXT class -----------------------------------------
//
// A heredoc BODY line genuinely begins with `ksail` while executing nothing.
// These two cases PASSED the predecessor: the decoy body was the only line it
// matched, so it read `nsa,mitre` from text that never runs while the real scan
// covered one framework. Measured on the shipped bash guard before this rewrite.
func TestRejectsHeredocBodyDecoy(t *testing.T) {
	cases := map[string]string{
		"heredoc body with an env-wrapped real scan": "cat <<'DOC' > /dev/null\n" +
			goodScan + "\n" +
			"DOC\n" +
			"env ksail workload scan --framework nsa --compliance-threshold 95",
		"heredoc body with the real scan in a function": "run_scan() { ksail workload scan --framework nsa; }\n" +
			"cat <<'DOC' > /dev/null\n" +
			goodScan + "\n" +
			"DOC\n" +
			"run_scan",
		"indented heredoc body": "cat <<-'DOC' > /dev/null\n" +
			"\t" + goodScan + "\n" +
			"\tDOC\n" +
			"env ksail workload scan --framework nsa",
	}
	for name, body := range cases {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a heredoc body executes nothing", name)
		}
	}
}

// A heredoc must not swallow the rest of the block: a real invocation AFTER a
// closed heredoc is still found. Without this, skipping heredocs could hide the
// genuine scan and turn the guard into a permanent fail-closed.
func TestHeredocDoesNotSwallowLaterInvocation(t *testing.T) {
	body := "cat <<'DOC' > /dev/null\nirrelevant text\nDOC\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected the invocation after the heredoc to be found, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set after heredoc = %q", got)
	}
}

// --- Structural parsing: YAML context ----------------------------------------
//
// A mention outside an executable `run:` scalar is not an invocation. The
// predecessor read raw file text, where a step `name:` and a quoted `with:`
// value both matched.
func TestIgnoresNonRunScalars(t *testing.T) {
	workflow := "jobs:\n  validate:\n    steps:\n" +
		"      - name: " + goodScan + "\n" +
		"        with:\n          command: " + goodScan + "\n" +
		"        run: echo unrelated\n"
	path := writeTemp(t, workflow)
	if _, err := frameworkSet(path); err == nil {
		t.Fatal("expected FAIL CLOSED: a name/with mention is not an executable invocation")
	}
}

func TestReadsRunScalarStructurally(t *testing.T) {
	scalars, err := runScalars([]byte(runBlock(goodScan)))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(scalars) != 1 || !strings.Contains(scalars[0], "ksail workload scan") {
		t.Fatalf("expected exactly one run scalar carrying the scan, got %q", scalars)
	}
}

// Unparseable YAML must fail closed, never be treated as "no findings".
func TestFailsClosedOnUnparseableYAML(t *testing.T) {
	path := writeTemp(t, "jobs: [unclosed\n")
	if _, err := frameworkSet(path); err == nil {
		t.Fatal("expected FAIL CLOSED on unparseable YAML")
	}
}

func TestFailsClosedOnMissingFile(t *testing.T) {
	if _, err := frameworkSet(t.TempDir() + "/absent.yaml"); err == nil {
		t.Fatal("expected FAIL CLOSED on a missing workflow")
	}
}

func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0o600)
}

var osWriteFile = os.WriteFile

// A non-`run:` BLOCK SCALAR whose line genuinely begins with `ksail` is the case
// the first-token filter cannot catch: after trimming, it is indistinguishable
// from a real invocation. Only reading `run:` scalars structurally excludes it.
// This is the YAML-context axis, distinct from the shell-context axis above.
func TestIgnoresKsailLineInNonRunBlockScalar(t *testing.T) {
	workflow := "env:\n  SCAN_HINT: |\n    " + goodScan + "\n" +
		"jobs:\n  validate:\n    steps:\n      - name: scan\n        run: |\n" +
		"          echo unrelated\n"
	path := writeTemp(t, workflow)
	if _, err := frameworkSet(path); err == nil {
		t.Fatal("expected FAIL CLOSED: a ksail line in a non-run block scalar executes nothing")
	}
}

// --- Wiring: a guard nothing calls protects nothing ---------------------------
//
// THESE ASSERTIONS READ PARSED `run:` SCALARS, NEVER RAW FILE TEXT. A raw grep
// for the validator's path is satisfied by any MENTION, and both workflows carry
// one: validate-main.yaml has a comment naming it beside the scan step. Deleting
// the real step once left such an assertion green while a direct push to main
// went unguarded — the exact gap the assertion claims to pin.

func repoWorkflow(t *testing.T, name string) string {
	t.Helper()
	return "../../.github/workflows/" + name
}

func runValuesOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("wiring: %s could not be read: %v", path, err)
	}
	scalars, err := runScalars(data)
	if err != nil {
		t.Fatalf("wiring: %s could not be parsed: %v", path, err)
	}
	return strings.Join(scalars, "\n")
}

// The real workflows must satisfy the guard — this is what would have caught the
// nsa-only baseline before it shipped.
func TestRealWorkflowsSatisfyTheGuard(t *testing.T) {
	for _, name := range []string{"ci.yaml", "validate-main.yaml"} {
		set, err := frameworkSet(repoWorkflow(t, name))
		if err != nil {
			t.Fatalf("wiring: the real %s does not satisfy the guard: %v", name, err)
		}
		if err := checkRequired(name, set); err != nil {
			t.Fatalf("wiring: %s: %v", name, err)
		}
	}
}

// The two real workflows must agree EXACTLY, or PR findings never persist as
// main-branch alerts.
func TestRealWorkflowsAgreeExactly(t *testing.T) {
	ci, err := frameworkSet(repoWorkflow(t, "ci.yaml"))
	if err != nil {
		t.Fatalf("wiring: %v", err)
	}
	main, err := frameworkSet(repoWorkflow(t, "validate-main.yaml"))
	if err != nil {
		t.Fatalf("wiring: %v", err)
	}
	if strings.Join(ci, ",") != strings.Join(main, ",") {
		t.Fatalf("wiring: framework sets differ — ci.yaml has [%s], validate-main.yaml has [%s]",
			strings.Join(ci, ","), strings.Join(main, ","))
	}
}

// Both workflows must RUN the validator in an executable step, and ci.yaml must
// run its tests — otherwise the guard could be widened with every check green.
func TestBothWorkflowsRunTheValidator(t *testing.T) {
	ci := runValuesOf(t, repoWorkflow(t, "ci.yaml"))
	main := runValuesOf(t, repoWorkflow(t, "validate-main.yaml"))

	if !strings.Contains(ci, "go run ./scripts/validate-kubescape-frameworks") {
		t.Fatal("wiring: no ci.yaml step RUNS the validator — an uncalled guard protects nothing")
	}
	if !strings.Contains(main, "go run ./scripts/validate-kubescape-frameworks") {
		t.Fatal("wiring: no validate-main.yaml step RUNS the validator — a direct push to main would be unchecked")
	}
	if !strings.Contains(ci, "go test ./scripts/validate-kubescape-frameworks") {
		t.Fatal("wiring: no ci.yaml step RUNS these tests — the guard could be widened with every check green")
	}
}

// The paired NEGATIVE arm: a workflow that only MENTIONS the validator — in a
// step name and a YAML comment, with no step running it — must not satisfy the
// wiring check.
func TestMentionOutsideExecutableStepDoesNotSatisfyWiring(t *testing.T) {
	decoy := "# go run ./scripts/validate-kubescape-frameworks enforces the match.\n" +
		"jobs:\n  validate:\n    steps:\n" +
		"      - name: go run ./scripts/validate-kubescape-frameworks\n" +
		"        run: echo unrelated\n"
	path := writeTemp(t, decoy)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading decoy: %v", err)
	}
	if !strings.Contains(string(data), "go run ./scripts/validate-kubescape-frameworks") {
		t.Fatal("decoy: the fixture must contain the mention it is testing")
	}
	scalars, err := runScalars(data)
	if err != nil {
		t.Fatalf("parsing decoy: %v", err)
	}
	if strings.Contains(strings.Join(scalars, "\n"), "go run ./scripts/validate-kubescape-frameworks") {
		t.Fatal("decoy: a comment/step-name mention satisfied the wiring check — deleting the real step would go unnoticed")
	}
}
