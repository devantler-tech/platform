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
		if err := checkRequired(set); err == nil {
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
	if err := checkRequired(set); err == nil {
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
		// A heredoc delimiter is a shell WORD, not an identifier. These two open a
		// heredoc in bash — measured — but matched no branch of the opener pattern,
		// so NO heredoc was recorded and the body was read as executable code. The
		// guard then took `nsa,mitre` from text that never runs, while the only scan
		// that executes covers one framework. A FAIL-OPEN, which is the direction
		// that matters: a delimiter mis-read as a shorter PREFIX (`<<EOF-1` -> `EOF`)
		// instead swallows the rest of the block and fails closed.
		"heredoc delimiter starting with a digit": "cat <<1EOF > /dev/null\n" +
			goodScan + "\n" +
			"1EOF\n" +
			"env ksail workload scan --framework nsa",
		"escaped heredoc delimiter": "cat <<\\EOF > /dev/null\n" +
			goodScan + "\n" +
			"EOF\n" +
			"env ksail workload scan --framework nsa",
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
		if err := checkRequired(set); err != nil {
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

// --- Bypasses that survived the structural rewrite ---------------------------
//
// Both decoys below pair a PASSING decoy with a REAL reduced scan hidden in a
// form the token filter deliberately skips (`env ksail ...`). That pairing is
// the whole point: neither decoy is dangerous alone, because supplying
// `nsa,mitre` where nothing runs changes no behaviour. They are dangerous
// because the decoy becomes the SOLE set the guard reads while the gate that
// actually executes scans less — the guard reports OK on a reduced scan, which
// is precisely the regression-that-looks-like-an-improvement it exists to catch.

// A mapping key named `run` is not necessarily a shell step. An action input
// spelled `with: run:` is a plain string the runner hands to the action, and
// reading it as an executable step lets a non-executing value satisfy the gate.
func TestActionInputNamedRunIsNotAnExecutableStep(t *testing.T) {
	workflow := "jobs:\n  validate:\n    steps:\n" +
		"      - uses: some/action@v1\n" +
		"        with:\n          run: " + goodScan + "\n" +
		"      - name: real\n        run: env ksail workload scan --framework nsa\n"
	path := writeTemp(t, workflow)
	if _, err := frameworkSet(path); err == nil {
		t.Fatal("expected FAIL CLOSED: a `with: run:` action input executes nothing, " +
			"supply the framework set while the real scan runs reduced")
	}
}

// A trailing shell comment is not part of the command. Reading the line as one
// substring lets comment text on an unrelated `ksail` line supply the set.
func TestInlineCommentDoesNotSupplyTheFrameworkSet(t *testing.T) {
	body := "ksail --version # workload scan --framework nsa,mitre\n" +
		"env ksail workload scan --framework nsa\n"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected FAIL CLOSED: `--framework` inside a trailing comment executes nothing, " +
			"supply the framework set while the real scan runs reduced")
	}
}

// Comment-stripping alone does not close the class: this line is not a comment at
// all, so stripping leaves it whole — yet it still is not the scan command. Only
// requiring the first three tokens to BE `ksail workload scan` rejects it.
// Verified non-vacuous: relaxing that check back to a substring test makes this
// test fail, while the other two still pass.
func TestQuotedArgumentTextDoesNotSupplyTheFrameworkSet(t *testing.T) {
	body := "ksail --version --note \"workload scan\" --framework nsa,mitre\n" +
		"env ksail workload scan --framework nsa\n"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected FAIL CLOSED: a `ksail` line whose command is not `workload scan` must not " +
			"supply the framework set while the real scan runs reduced")
	}
}

// The comment must be removed even on a line that IS the scan. Here the real
// command carries no `--framework` at all, so the flag the guard reads comes
// entirely from the comment — the scan runs on whatever default applies while
// the guard reports the comment's set.
//
// Verified non-vacuous: making stripComment a no-op makes this test fail, while
// the command-shape tests still pass. It is the only fixture that isolates it.
func TestCommentCannotSupplyAMissingFrameworkFlag(t *testing.T) {
	body := "ksail workload scan --compliance-threshold 95 # --framework nsa,mitre\n"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected FAIL CLOSED: the executed command has no --framework, " +
			"so a commented-out one must not satisfy the gate")
	}
}

// --- Round two: the LINE was still read with string operations ------------------
//
// The two below are the same class as the first round — a decoy supplying the set
// while a real reduced scan executes — but they defeat the *scanner* rather than
// the command-shape test. Both were found by review after the shape fix landed,
// which is the signal to stop closing spellings and parse the line instead.

// `strings.Count` matched one exact spelling, so re-spacing the second command
// hid it: the line then read as a single invocation and the FIRST (full)
// framework list was accepted, while the later reduced scan is the one whose
// SARIF is uploaded.
func TestChainedScanIsRejectedRegardlessOfSpacing(t *testing.T) {
	body := "ksail workload scan --framework nsa,mitre -o throwaway.sarif && " +
		"ksail  workload  scan --framework nsa -o kubescape.sarif\n"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected FAIL CLOSED: two chained scans are ambiguous however they are spaced, " +
			"so the guard must not judge the upload on the first one's framework list")
	}
}

// `<<` inside a QUOTED argument is an ordinary filename, not a heredoc operator.
// Treating it as one suppressed every following line until a matching word, which
// swallowed the real reduced scan while the shell executed both.
func TestQuotedRedirectionTargetDoesNotOpenAHeredoc(t *testing.T) {
	body := "ksail workload scan --framework nsa,mitre -o '<<true'\n" +
		"ksail workload scan --framework nsa -o kubescape.sarif\n" +
		"true\n"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected FAIL CLOSED: `<<` inside quotes is a filename, so the second scan " +
			"must remain visible and make this ambiguous")
	}
}

// --- The CONTROL-FLOW class -------------------------------------------------
//
// `shellSplit` returns every `&&`/`||` segment as an independent command, so a
// segment the shell would SKIP was counted as an executed invocation. That turns
// a never-executing decoy into the guard's sole evidence: pair it with a real
// reduced scan the shape test already ignores (`env ksail ...`) and the decoy
// supplies a full framework list while the reduced scan is what actually runs.
//
// The fix is to REJECT rather than to evaluate. Whether `&&` fires depends on a
// command's exit status, which is not decidable from the text — so a scan whose
// execution is conditional is a form this guard cannot read, and the fail-closed
// direction is the correct one.
func TestRejectsConditionallySkippedScan(t *testing.T) {
	cases := map[string]string{
		"and-guarded decoy hides an env-wrapped reduced scan": "false && " + goodScan + " -o kubescape.sarif\n" +
			"env ksail workload scan --framework nsa -o kubescape.sarif\n",
		"or-guarded decoy hides an env-wrapped reduced scan": "true || " + goodScan + " -o kubescape.sarif\n" +
			"env ksail workload scan --framework nsa -o kubescape.sarif\n",
		"and-guarded decoy alone": "false && " + goodScan + "\n",
	}
	for name, body := range cases {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a conditionally executed scan is not evidence "+
				"of what the gate actually runs", name)
		}
	}
}

// The control for the rejection above: a scan that is FIRST on its line executes
// unconditionally, and a conditional segment that is not a scan changes nothing.
// Without this, "reject any line containing `&&`" would pass the test above while
// breaking every legitimate invocation.
func TestUnconditionalScanWithTrailingOperatorIsAccepted(t *testing.T) {
	cases := map[string]string{
		"scan then and-guarded echo": goodScan + " && echo done",
		"scan then or-guarded exit":  goodScan + " || exit 1",
		"scan then sequenced echo":   goodScan + " ; echo done",
	}
	for name, body := range cases {
		got, err := setOf(t, body)
		if err != nil {
			t.Fatalf("%s: expected ACCEPT — the scan itself runs unconditionally, got: %v", name, err)
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Fatalf("%s: normalised set = %q, want %q", name, strings.Join(got, ","), "mitre,nsa")
		}
	}
}

// --- The COMPOUND-COMMAND class ---------------------------------------------
//
// Rejecting `&&`/`||` closed one way to make a scan unreachable. It is not the only
// one: an `if` body and a function body are both skipped without any operator on the
// scan's own line, so each was counted as an executed invocation. Paired with a real
// reduced scan the shape test ignores (`env ksail ...`), the unreachable full-framework
// command becomes the guard's sole evidence — the identical outcome, reached by a
// different spelling.
//
// THAT REPETITION IS THE SIGNAL. Two rounds closing two spellings of one class means the
// blacklist is the wrong shape: `while`, `for`, `case`, a subshell and a brace group are
// all still open, and each would arrive as its own round. So this INVERTS — a scan is
// accepted only from a scalar that is a plain sequence of simple commands, and any
// compound-command construct in that scalar refuses. Deciding reachability properly
// needs a shell parser, which this guard deliberately does not implement.
func TestRejectsScanInsideCompoundCommand(t *testing.T) {
	reducedScan := "env ksail workload scan --framework nsa --compliance-threshold 95"
	cases := map[string]string{
		"if body":       "if false; then\n  " + goodScan + "\nfi\n" + reducedScan,
		"function body": "unused() {\n  " + goodScan + "\n}\n" + reducedScan,
		"while body":    "while false; do\n  " + goodScan + "\ndone\n" + reducedScan,
		"for body":      "for i in 1; do\n  " + goodScan + "\ndone\n" + reducedScan,
		"case body":     "case x in\n  y)\n    " + goodScan + "\n    ;;\nesac\n" + reducedScan,
		"brace group":   "{\n  " + goodScan + "\n}\n" + reducedScan,
	}
	for name, body := range cases {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a scan inside a compound command is not "+
				"evidence of what the gate actually runs", name)
		}
	}
}

// The control for the inversion: the shipped shape is a plain sequence of simple
// commands and must still be accepted. Without this, "reject any scalar containing a
// reserved word" could be satisfied by rejecting everything.
func TestPlainCommandSequenceIsStillAccepted(t *testing.T) {
	body := "go test ./scripts/generate-kubescape-exceptions\n" +
		"go run ./scripts/generate-kubescape-exceptions -o /tmp/kubescape-exceptions.json\n" +
		"ksail workload scan --framework nsa,mitre --exceptions /tmp/kubescape-exceptions.json " +
		"--compliance-threshold 95 --format sarif -o \"${RUNNER_TEMP}/kubescape.sarif\"\n"
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected ACCEPT — this is the shipped shape, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("normalised set = %q, want %q", strings.Join(got, ","), "mitre,nsa")
	}
}

// compoundToken must recognise a function-definition head WHATEVER the spacing.
//
// 🔴 THIS IS A UNIT TEST ON PURPOSE, and the reason is worth recording: an end-to-end
// fixture cannot isolate it. A function body structurally contributes a closing `}` as
// its own segment, and `}` is already a compound word — so `unused(){ … }` is refused
// through the CLOSER whether or not the opener is recognised. Every integration case I
// wrote for this passed against the old suffix-only test, i.e. proved nothing about the
// change.
//
// The defect is therefore in the helper's own contract rather than in the guard's current
// verdict: `compoundToken` promises to return the token introducing a compound command,
// and for `unused(){` — which bash accepts, brace and all, as ONE field — it returned "".
// Closing it makes the coverage principled instead of dependent on a closer showing up.
//
// Ablation: restoring `strings.HasSuffix(head, "()")` fails this test on `unused(){`, and
// fails NO integration case.
func TestCompoundTokenRecognisesGroupingHeads(t *testing.T) {
	grouping := []string{
		"unused(){",  // bash accepts no space before the brace; arrives as one field
		"unused()",   // the spaced form, `unused() {`
		"unused(){}", // an empty body, still one field
		"{",          // a brace group
		"}",
		"(", // a subshell
		")",
		"if",
		"done",
	}
	for _, tok := range grouping {
		if got := compoundToken(tok); got == "" {
			t.Errorf("compoundToken(%q) = \"\", want it recognised as a compound-command head", tok)
		}
	}

	// The control: these characters are legitimate INSIDE a quoted command name, and a
	// shape test that ignored quoting would refuse an ordinary invocation such as
	// `"${RUNNER_TEMP}/bin/setup" --quiet`. Plain command names must stay accepted too.
	plain := []string{
		`"${RUNNER_TEMP}/bin/setup"`,
		`'{literal}'`,
		"ksail",
		"go",
		"env",
		"echo",
	}
	for _, tok := range plain {
		if got := compoundToken(tok); got != "" {
			t.Errorf("compoundToken(%q) = %q, want \"\" — this is an ordinary command name", tok, got)
		}
	}
}

// A command line may carry MORE THAN ONE heredoc redirection, and every body that
// follows is non-executing input in the order the delimiters appear. Tracking only
// the first delimiter stops the skip at the first terminator, so the SECOND body's
// lines are read as executing code.
//
// Verified against bash directly: in the fixture below the line between FIRST and
// SECOND does not run, and only the trailing `env ksail ...` scan executes. That
// scan omits `mitre`, so the guard must FAIL CLOSED. Accepting it would mean the
// gate reports the required framework set while the real scan checks less — a
// fail-open on the control this validator exists to be.
func TestRejectsSecondHeredocBodyDecoy(t *testing.T) {
	body := "cat <<'FIRST' <<'SECOND' >/dev/null\n" +
		"ignored\n" +
		"FIRST\n" +
		goodScan + "\n" +
		"SECOND\n" +
		"env ksail workload scan --framework nsa --compliance-threshold 95"
	if got, err := setOf(t, body); err == nil {
		t.Fatalf("expected FAIL CLOSED — the second heredoc body executes nothing; got set %q", got)
	}
}

// The other direction of the same bug: a valid non-identifier delimiter must
// TERMINATE correctly. `<<EOF-1` used to match the prefix `EOF`, whose terminator
// never arrives, so every later line was swallowed as heredoc body and the real
// scan vanished — a false reject that reads exactly like a clean run.
func TestNonIdentifierHeredocTerminatesCorrectly(t *testing.T) {
	body := "cat <<EOF-1 > /dev/null\nirrelevant text\nEOF-1\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected the scan after an `EOF-1` heredoc to be found, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set after non-identifier heredoc = %q", got)
	}
}

// `<<<` is a here-STRING: it consumes a word on its own line and swallows no body.
// Routed through the heredoc path it parsed as a heredoc whose delimiter was the
// quoted word, silently eating the rest of the block.
func TestHereStringDoesNotSwallowLaterInvocation(t *testing.T) {
	body := "grep -q x <<<\"some text\"\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected the scan after a here-string to be found, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set after here-string = %q", got)
	}
}

// A `<<` whose delimiter cannot be parsed leaves the body boundary unknown, so every
// following line has undecidable status. That fails CLOSED rather than being walked
// past — walking past it is what handed a decoy body to the scanner as code.
func TestUnreadableHeredocOpenerFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"empty delimiter":    "cat << > /dev/null\n" + goodScan,
		"unterminated quote": "cat <<'EOF > /dev/null\n" + goodScan,
		"trailing backslash": "cat <<\\",
	} {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — an unreadable `<<` leaves the body boundary unknown", name)
		}
	}
}

// Legacy backtick command substitution SPANS LINES, so whether a command inside one
// executes is not a fact about its own line. `false &&` before the newline suppresses
// the scan on the next line, yet the validator read that line as an unconditional bare
// invocation and took `nsa,mitre` from it while the only scan that ran covered one
// framework. Verified in bash: the backticked scan produces no output, and the same
// fixture with `true &&` does.
//
// Recorded for the WHOLE scalar and acted on only if a scan is found, exactly like the
// compound-command rule: an ordinary `run:` block that never invokes the scan may use
// whatever shell it likes.
func TestRejectsScanInsideBacktickSubstitution(t *testing.T) {
	body := "echo `false &&\n" + goodScan + "\n`\nenv ksail workload scan --framework nsa"
	if set, err := setOf(t, body); err == nil {
		t.Fatalf("expected FAIL CLOSED — a scan inside a backtick substitution is not decidable from its own line; got %q", set)
	}
}

// A backtick in a block that never invokes the scan is not this guard's business.
func TestBacktickWithoutScanIsAccepted(t *testing.T) {
	body := "echo `date`\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected a block whose backtick carries no scan to be read normally, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// `$(...)` SPANS LINES exactly as a backtick does, and the double-quoted form is the
// one the backtick rule never reached: the closing `)"` on the following line is not a
// backtick, and the scan's own line begins with `ksail`, so no compound token is seen
// either. `false &&` before the newline suppresses the scan while that line still reads
// as an unconditional invocation, and the guard took `nsa,mitre` from it while the only
// scan bash actually ran covered one framework.
//
// MEASURED, not reasoned: with a `ksail` stub on PATH this exact fixture emits only the
// reduced `--framework nsa` invocation, and the validator returned `["mitre" "nsa"]`
// with a nil error before this rule existed.
//
// Recorded for the WHOLE scalar and acted on only if a scan is found, exactly like the
// backtick and compound-command rules.
func TestRejectsScanInsideDollarSubstitution(t *testing.T) {
	cases := map[string]string{
		// Inside double quotes -- the measured fail-open.
		"double-quoted": "echo \"$(false &&\n" + goodScan +
			" --compliance-threshold 95)\"\nenv ksail workload scan --framework nsa",
		// Unquoted. MEASURED: this arm is carried by the pre-existing COMPOUND-command
		// rule, not by the substitution rule -- neutralising the substitution rejection
		// leaves it passing. It is pinned so that rule cannot silently stop covering it.
		"unquoted": "echo $(false &&\n" + goodScan +
			" --compliance-threshold 95)\nenv ksail workload scan --framework nsa",
	}
	for name, body := range cases {
		if set, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a scan inside a $() substitution that spans lines is not decidable from its own line; got %q", name, set)
		}
	}
}

// PARITY OF INTENT WITH THE BACKTICK RULE: a substitution CLOSED on its own line cannot
// suppress anything later, so it must not be refused. Without this the rule would reject
// an ordinary `echo "$(date)"` and turn the guard into a permanent fail-closed.
func TestClosedDollarSubstitutionIsAccepted(t *testing.T) {
	cases := map[string]string{
		"unquoted":      "echo $(date)\n" + goodScan,
		"double-quoted": "echo \"$(date)\"\n" + goodScan,
		"nested":        "echo \"$(dirname \"$(pwd)\")\"\n" + goodScan,
		"arithmetic":    "echo \"$((1 + 2))\"\n" + goodScan,
		// A single-quoted `$(` is literal text, never a substitution.
		"single-quoted": "echo '$(false &&'\n" + goodScan,
	}
	for name, body := range cases {
		got, err := setOf(t, body)
		if err != nil {
			t.Fatalf("%s: expected a closed substitution to be read normally, got: %v", name, err)
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Fatalf("%s: set = %q", name, got)
		}
	}
}

// BASH JOINS a line ending in an unquoted backslash to the next physical line, so an
// `&&` guard written on one line reaches the command on the next -- and a walk that
// treats every physical line as its own command cannot see it. The scan line then reads
// as an unconditional bare invocation and supplies `nsa,mitre` while the only scan that
// executes covers one framework.
//
// MEASURED: with a `ksail` stub on PATH the fixture below emits only the reduced
// `--framework nsa` invocation, and the validator returned ["mitre" "nsa"] with a nil
// error before continuations were joined.
//
// This is the same fail-open the `&&`/`||` rule already closes, reached through a
// different spelling -- which is why joining, rather than adding a third operator test,
// is the fix: it puts the real command line in front of the rule that already exists.
func TestRejectsScanContinuedFromAConditional(t *testing.T) {
	cases := map[string]string{
		"and": "false && \\\n" + goodScan + " --compliance-threshold 95\n" +
			"env ksail workload scan --framework nsa",
		"or": "true || \\\n" + goodScan + " --compliance-threshold 95\n" +
			"env ksail workload scan --framework nsa",
		// The guard is on the FIRST of two continued lines, so the scan is two joins away.
		"two continuations": "false && \\\n" + "echo x \\\n" + "&& " + goodScan + "\n" +
			"env ksail workload scan --framework nsa",
	}
	for name, body := range cases {
		if set, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a scan continued from a conditional does not run unconditionally; got %q", name, set)
		}
	}
}

// A continuation carrying NO conditional is ordinary shell style and must still be read.
// `ci.yaml` alone has 29 continued lines, so refusing the form outright -- rather than
// joining it -- would reject the real workflows this guard exists to validate.
func TestJoinsAnOrdinaryContinuationInsteadOfRefusing(t *testing.T) {
	body := "ksail workload scan \\\n  --framework nsa,mitre \\\n  --compliance-threshold 95"
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected a continued scan invocation to be read as one command, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// An EVEN number of trailing backslashes is escaped backslashes, not a continuation, so
// the line ends and the next one stands alone. Keyed on parity for the same reason the
// backtick rule is.
func TestEscapedBackslashDoesNotContinueTheLine(t *testing.T) {
	body := "echo a\\\\\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected an escaped trailing backslash not to swallow the next line, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// A QUOTED ARGUMENT MAY SPAN PHYSICAL LINES, and `inSingle`/`inDouble` were reset on
// every line, so the lines inside one were parsed as executable commands. Bash PRINTS
// the decoy and executes only the reduced scan; the validator accepted the block.
// Measured before the fix: set=["mitre" "nsa"], err=nil.
func TestRejectsScanInsideAMultilineQuotedArgument(t *testing.T) {
	cases := map[string]string{
		"single": "printf '%s\\n' '\n" + goodScan + "\n'\n" +
			"env ksail workload scan --framework nsa",
		"double": "printf '%s\\n' \"\n" + goodScan + "\n\"\n" +
			"env ksail workload scan --framework nsa",
	}
	for name, body := range cases {
		if set, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — text inside a multiline quoted argument is printed, not executed; got %q", name, set)
		}
	}
}

// A real command written AFTER the closing quote is still executed, so it must still be
// read. Without this the fix could pass by discarding the remainder of every line that
// closes a quote.
func TestReadsACommandAfterAClosingQuote(t *testing.T) {
	body := "printf '%s\\n' '\nirrelevant text\n' && true\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected the invocation after the quoted block to be found, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// `<<-` strips leading TABS from the terminator, never SPACES -- measured in bash: a
// space-indented `DOC` does NOT end the heredoc. Trimming spaces too ended the body
// early, so lines bash still treats as input were read as code and supplied the set.
func TestSpaceIndentedDedentTerminatorDoesNotEndTheHeredoc(t *testing.T) {
	body := "cat <<-DOC > /dev/null\n\tirrelevant\n  DOC\n" +
		goodScan + "\nDOC\nenv ksail workload scan --framework nsa"
	if set, err := setOf(t, body); err == nil {
		t.Fatalf("expected FAIL CLOSED — a space-indented terminator does not end a <<- heredoc in bash; got %q", set)
	}
}

// ...and a TAB-indented terminator DOES end it, so the fix must not swallow the rest of
// the block. Without this, stripping nothing at all would also pass the case above.
func TestTabIndentedDedentTerminatorStillEndsTheHeredoc(t *testing.T) {
	body := "cat <<-DOC > /dev/null\n\tirrelevant\n\tDOC\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected a tab-indented terminator to end the heredoc, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// A workflow-level `if:` is evaluated BEFORE any shell starts, so no shell parsing can
// see it. A step-level one is refused outright (no real scan step carries one), and a
// CONSTANT-FALSE job condition is refused too.
func TestRejectsScanInAConditionalStep(t *testing.T) {
	workflow := "jobs:\n  validate:\n    steps:\n" +
		"      - if: ${{ false }}\n        run: " + goodScan + "\n" +
		"      - run: env ksail workload scan --framework nsa\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — a step the runner skips cannot supply the framework set; got %q", set)
	}
}

func TestRejectsScanInAConstantFalseJob(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ false }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — a constant-false job never runs; got %q", set)
	}
}

// A job carrying a REAL condition is NOT refused: the live `validate` job gates on a
// path filter, and refusing every conditional job rejected all three real workflows --
// measured. This pins that the narrowing stays.
func TestRealConditionalJobIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: github.event_name == 'pull_request'\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("expected a genuinely conditional job to be read, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}
