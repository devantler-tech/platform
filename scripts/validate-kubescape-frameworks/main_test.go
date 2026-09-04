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
		// A TERMINATOR IS COMPARED EXACTLY. Bash does not accept trailing spaces or
		// tabs on the delimiter line, but the guard trimmed them -- so `DOC   ` ended
		// the body HERE while bash kept reading, and the decoy below it was scanned as
		// code. Reproduced end to end against bash before this was fixed: bash printed
		// the decoy as heredoc TEXT and executed only the reduced scan, while the guard
		// reported both frameworks covered.
		"terminator with trailing whitespace": "cat <<DOC > /dev/null\n" +
			"hello\n" +
			"DOC   \n" +
			goodScan + "\n" +
			"DOC\n" +
			"env ksail workload scan --framework nsa",
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
			"so it must not supply the framework set while the real scan runs reduced")
	}
}

// A trailing shell comment is not part of the command. Reading the line as one
// substring lets comment text on an unrelated `ksail` line supply the set.
func TestInlineCommentDoesNotSupplyTheFrameworkSet(t *testing.T) {
	body := "ksail --version # workload scan --framework nsa,mitre\n" +
		"env ksail workload scan --framework nsa\n"
	if _, err := setOf(t, body); err == nil {
		t.Fatal("expected FAIL CLOSED: `--framework` inside a trailing comment executes nothing, " +
			"so it must not supply the framework set while the real scan runs reduced")
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

// Three ways a reduced scan executes beside the counted one while reading as
// something else on the unconditional path: a brace-expanded option (`--{framework=…}`
// carries no literal flag until the shell expands it), a command string handed to an
// executing shell (`bash -c '…'`, `sh -c "…"`, `eval '…'` — one quoted argument, so the
// consecutive-token rules never see the invocation), and a command whose every word is
// a variable assigned earlier in the block. Each is refused as undecidable.
func TestRejectsReducedScanHiddenFromTheUnconditionalPath(t *testing.T) {
	cases := map[string]string{
		"brace-expanded option": goodScan + "\nksail workload scan --{framework=nsa,output=kubescape.sarif}",
		"bash -c single-quoted": goodScan + "\nbash -c 'ksail workload scan --framework nsa -o kubescape.sarif'",
		"sh -c double-quoted":   goodScan + "\nsh -c \"ksail workload scan --framework nsa -o kubescape.sarif\"",
		"eval":                  goodScan + "\neval 'ksail workload scan --framework nsa -o kubescape.sarif'",
		// The command-string option after OTHER interpreter options executes the
		// string exactly as a bare -c does.
		"bash -e -c":                         goodScan + "\nbash -e -c 'ksail workload scan --framework nsa -o kubescape.sarif'",
		"sh -e -c":                           goodScan + "\nsh -e -c \"ksail workload scan --framework nsa -o kubescape.sarif\"",
		"bash -o operand before -c":          goodScan + "\nbash -o errexit -c 'ksail workload scan --framework nsa -o kubescape.sarif'",
		"sh -o operand before -c":            goodScan + "\nsh -o errexit -c \"ksail workload scan --framework nsa -o kubescape.sarif\"",
		"bash --rcfile operand before -c":    goodScan + "\nbash --rcfile ./bashrc -c 'ksail workload scan --framework nsa -o kubescape.sarif'",
		"bash --noprofile -c":                goodScan + "\nbash --noprofile -c 'ksail workload scan --framework nsa -o kubescape.sarif'",
		"bash -ec cluster":                   goodScan + "\nbash -ec 'ksail workload scan --framework nsa -o kubescape.sarif'",
		"all words from variables":           "KSAIL=ksail WORKLOAD=workload SCAN=scan\n" + goodScan + "\n\"$KSAIL\" \"$WORKLOAD\" \"$SCAN\" --framework nsa -o kubescape.sarif",
		"all words from variables, unquoted": "KSAIL=ksail WORKLOAD=workload SCAN=scan\n" + goodScan + "\n$KSAIL $WORKLOAD $SCAN --framework nsa -o kubescape.sarif",
	}
	for name, body := range cases {
		if set, err := setOf(t, body); err == nil {
			t.Errorf("%s: expected FAIL CLOSED — a reduced scan executes beside the counted one; got %q", name, set)
		}
	}
}

// The controls: an executing shell handed text with no scan word, and a variable that
// is merely an ARGUMENT of a plainly spelled scan, are ordinary shell.
func TestExecutingShellWithoutAScanWordIsStillIgnored(t *testing.T) {
	cases := map[string]string{
		"bash -c without a scan word": goodScan + "\nbash -c 'echo hello'",
		"eval without a scan word":    goodScan + "\neval 'true'",
		// A script path is not a command string: the options end at the first
		// non-option that is not an option operand, so a scan word in a later
		// argument is not this rule's business.
		"bash -e running a script":         goodScan + "\nbash -e ./scripts/report.sh scan",
		"bash -o operand running a script": goodScan + "\nbash -o errexit ./scripts/report.sh scan",
	}
	for name, body := range cases {
		got, err := setOf(t, body)
		if err != nil {
			t.Errorf("%s: expected ACCEPT; got: %v", name, err)
			continue
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Errorf("%s: set = %q", name, got)
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
	// The trailing scan is written BARE rather than `env ksail ...`. The token filter
	// skips an env-prefixed line, so with that spelling nothing in this fixture counts
	// as an invocation and the case passes through the zero-invocation error either
	// way -- it could not tell "the heredoc body was ignored" from "no invocation was
	// found at all". Asserting the returned SET pins both halves: the decoy inside the
	// body was not read, and the scan that really executes was.
	body := "cat <<-DOC > /dev/null\n\tirrelevant\n  DOC\n" +
		goodScan + "\nDOC\nksail workload scan --framework nsa"
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected the trailing reduced scan to be read, got: %v", err)
	}
	if strings.Join(got, ",") != "nsa" {
		t.Fatalf("set = %q, want %q — a space-indented terminator does not end a <<- heredoc in bash, so the body must not be read as code",
			strings.Join(got, ","), "nsa")
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

// The conditional path used to detect the scan by raw substring, so a quoted, escaped or
// expanded spelling of the command word in a conditional step read as ordinary shell and
// the step was skipped rather than refused — executing a reduced scan beside the counted
// full one. It now reads the scalar token by token like the primary path, so every
// spelling that path counts or refuses is refused here too.
func TestRejectsNormalisedScanSpellingInAConditionalStep(t *testing.T) {
	cases := map[string]string{
		"double-quoted fragment in the command word": "k\"s\"ail workload scan --framework nsa",
		"backslash escape in the command word":       "k\\sail workload scan --framework nsa",
		"ANSI-C fragment in the command word":        "k$'s'ail workload scan --framework nsa",
		"fully quoted command word behind a prefix":  "env 'ksail' workload scan --framework nsa",
		"constructed option word":                    "ksail workload scan --frame${SUFFIX} nsa",
		"constructed workload and option word":       "ksail work${LOAD} scan --frame${SUFFIX} nsa",
		"every command word expanded":                "${ksail} ${workload} ${scan} --framework nsa",
	}
	for name, line := range cases {
		workflow := "jobs:\n  validate:\n    steps:\n" +
			"      - run: " + goodScan + "\n" +
			"      - if: ${{ github.ref == 'refs/heads/main' }}\n        run: " + line + "\n"
		path := writeTemp(t, workflow)
		set, err := frameworkSet(path)
		if err == nil {
			t.Errorf("%s: expected FAIL CLOSED — the conditional step can execute a scan the guard never validated; got %q", name, set)
			continue
		}
		if !strings.Contains(err.Error(), "guarded by a workflow-level `if:`") {
			t.Errorf("%s: the refusal must name the conditional step, proving this rule fired; got: %v", name, err)
		}
	}
}

// A backslash continuation splits the command words across physical lines; the
// conditional check joins them before reading, so the split scan is still refused.
func TestRejectsContinuedScanInAConditionalStep(t *testing.T) {
	workflow := "jobs:\n  validate:\n    steps:\n" +
		"      - run: " + goodScan + "\n" +
		"      - if: ${{ github.ref == 'refs/heads/main' }}\n        run: |\n" +
		"          ksail workload \\\n            scan --framework nsa\n"
	path := writeTemp(t, workflow)
	set, err := frameworkSet(path)
	if err == nil {
		t.Fatalf("expected FAIL CLOSED — a continued scan in a conditional step still executes; got %q", set)
	}
	if !strings.Contains(err.Error(), "guarded by a workflow-level `if:`") {
		t.Fatalf("the refusal must name the conditional step; got: %v", err)
	}
}

// A backslash at the end of a COMMENT does not continue it: the next physical line
// executes. Joining continuations before deciding what is a comment folded that line
// into the comment and skipped the scan it carried.
func TestRejectsScanAfterABackslashEndedCommentInAConditionalStep(t *testing.T) {
	workflow := "jobs:\n  validate:\n    steps:\n" +
		"      - run: " + goodScan + "\n" +
		"      - if: ${{ github.ref == 'refs/heads/main' }}\n        run: |\n" +
		"          # this comment does not continue \\\n          ksail workload scan --framework nsa\n"
	path := writeTemp(t, workflow)
	set, err := frameworkSet(path)
	if err == nil {
		t.Fatalf("expected FAIL CLOSED — the line after a backslash-ended comment still executes; got %q", set)
	}
	if !strings.Contains(err.Error(), "guarded by a workflow-level `if:`") {
		t.Fatalf("the refusal must name the conditional step; got: %v", err)
	}
}

// A quote inside a shell COMMENT is not a quote: the comment ends at its physical
// newline and bash never opens a string there. If the fold carried that quote state
// into the next line, the newline after the comment would become a space, the scan
// on the following line would be swallowed into the comment, and the conditional
// step would be skipped as prose — executing an unvalidated reduced scan.
func TestRejectsScanAfterACommentWithAnUnmatchedQuoteInAConditionalStep(t *testing.T) {
	cases := map[string]string{
		"unmatched double quote":         "          # unmatched quote: \"\n",
		"unmatched single quote":         "          # it's unmatched\n",
		"trailing comment after a word":  "          true # unmatched quote: \"\n",
		"quote in a comment then a word": "          # \" a\n          echo ok\n",
		// bash removes an unquoted backslash-newline before it reads the next line,
		// so `echo \` followed by `# …` is `echo # …`: the `#` starts a comment.
		"comment after an unquoted continuation, double quote": "          echo \\\n          # unmatched quote: \"\n",
		"comment after an unquoted continuation, single quote": "          echo \\\n          # it's unmatched\n",
		// A shell metacharacter ends a word, so `#` right after `;`, `|` or `&`
		// opens a comment with no space in between.
		"comment right after a semicolon, double quote": "          true;# unmatched quote: \"\n",
		"comment right after a pipe, single quote":      "          true|# it's unmatched\n",
		"comment right after an ampersand":              "          true&# unmatched quote: \"\n",
		// A continuation that lands on a `#` opens a comment; the backslash INSIDE
		// that comment continues nothing, so the scan on the third line executes.
		"backslash-ended comment after a continuation":                   "          echo \\\n          # this comment does not continue \\\n",
		"backslash-ended comment after a continuation, unmatched double": "          echo \\\n          # unmatched quote: \" \\\n",
		"backslash-ended comment after a continuation, unmatched single": "          echo \\\n          # it's unmatched \\\n",
	}
	for name, comment := range cases {
		workflow := "jobs:\n  validate:\n    steps:\n" +
			"      - run: " + goodScan + "\n" +
			"      - if: ${{ github.ref == 'refs/heads/main' }}\n        run: |\n" +
			comment +
			"          ksail workload scan --framework nsa\n"
		path := writeTemp(t, workflow)
		set, err := frameworkSet(path)
		if err == nil {
			t.Errorf("%s: expected FAIL CLOSED — the scan after a comment carrying a quote still executes; got %q", name, set)
			continue
		}
		if !strings.Contains(err.Error(), "guarded by a workflow-level `if:`") {
			t.Errorf("%s: the refusal must name the conditional step; got: %v", name, err)
		}
	}
}

// The control for the join: a continuation whose next line continues the WORD
// (`foo\` then `#bar`) is not a comment, so the join still happens and the step is
// ordinary prose — accepted, exactly as before.
func TestContinuedWordWithAHashIsStillJoinedInAConditionalStep(t *testing.T) {
	workflow := "jobs:\n  validate:\n    steps:\n" +
		"      - run: " + goodScan + "\n" +
		"      - if: ${{ github.ref == 'refs/heads/main' }}\n        run: |\n" +
		"          echo foo\\\n          #bar \\\n          true\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("expected ACCEPT — `echo foo#bar true` invokes no scan; got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("normalised set = %q, want %q", strings.Join(got, ","), "mitre,nsa")
	}
}

// The command words assigned on one line and expanded on the next: no single line
// spells the scan, so only scalar-wide evidence over the executable lines can see it.
func TestRejectsScanAssembledAcrossLinesInAConditionalStep(t *testing.T) {
	workflow := "jobs:\n  validate:\n    steps:\n" +
		"      - run: " + goodScan + "\n" +
		"      - if: ${{ github.ref == 'refs/heads/main' }}\n        run: |\n" +
		"          KSAIL=ksail WORKLOAD=workload SCAN=scan\n" +
		"          ${KSAIL} ${WORKLOAD} ${SCAN} --framework nsa\n"
	path := writeTemp(t, workflow)
	set, err := frameworkSet(path)
	if err == nil {
		t.Fatalf("expected FAIL CLOSED — the assembled scan executes in the conditional step; got %q", set)
	}
	if !strings.Contains(err.Error(), "guarded by a workflow-level `if:`") {
		t.Fatalf("the refusal must name the conditional step; got: %v", err)
	}
}

// The control: quoted prose in a conditional step is still prose, exactly as on the
// unconditional path — including prose that carries an expansion, which the
// scalar-wide evidence must not read as a scan assembled across lines.
func TestQuotedProseInAConditionalStepIsStillIgnored(t *testing.T) {
	cases := map[string]string{
		"single-quoted prose":                   "echo 'ksail workload scan --framework nsa'",
		"double-quoted prose with an expansion": "echo \"ksail workload scan --framework $STATUS\"",
		"double-quoted prose with a backtick":   "echo \"ksail workload scan --framework `date`\"",
		// The string spans two physical lines; the screen folds the quoted newline first.
		"multi-line single-quoted prose": "|\n          echo 'ksail workload\n          scan --framework nsa'",
		"multi-line double-quoted prose": "|\n          echo \"ksail workload\n          scan --framework $STATUS\"",
		// A backslash-newline INSIDE double quotes is removed by bash before the quoted
		// argument is built, so this is still one printed string — not a command whose
		// first line contributes `ksail workload` and whose second contributes the rest.
		"double-quoted prose continued with a backslash-newline":                "|\n          echo \"ksail workload \\\n          scan --framework $STATUS\"",
		"double-quoted prose continued with a backslash-newline and a backtick": "|\n          echo \"ksail workload \\\n          scan --framework `date`\"",
	}
	for name, line := range cases {
		workflow := "jobs:\n  validate:\n    steps:\n" +
			"      - run: " + goodScan + "\n" +
			"      - if: ${{ github.ref == 'refs/heads/main' }}\n        run: " + line + "\n"
		path := writeTemp(t, workflow)
		got, err := frameworkSet(path)
		if err != nil {
			t.Errorf("%s: expected ACCEPT — quoted prose invokes nothing; got: %v", name, err)
			continue
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Errorf("%s: normalised set = %q, want %q", name, strings.Join(got, ","), "mitre,nsa")
		}
	}
}

// collapseQuotedNewlines follows bash's line-continuation rule: a backslash-newline pair
// is removed inside double quotes and outside quotes alike, and kept literal only inside
// single quotes. Outside quotes the pair is left for the per-line continuation join in
// scanCandidate, which must see it on the physical line so a backslash that ends a
// COMMENT is not read as continuing that comment.
func TestCollapseQuotedNewlinesFoldsABackslashNewlineInsideDoubleQuotes(t *testing.T) {
	cases := map[string][2]string{
		"double-quoted":     {"echo \"ksail workload \\\nscan\"", "echo \"ksail workload scan\""},
		"single-quoted":     {"echo 'ksail workload \\\nscan'", "echo 'ksail workload \\ scan'"},
		"unquoted":          {"ksail workload \\\nscan", "ksail workload \\\nscan"},
		"escaped backslash": {"echo \"a\\\\\nb\"", "echo \"a\\\\ b\""},
		// A comment ends at its newline whatever quotes it carries, so the next line
		// keeps its own structure; a `#` inside a quoted string is not a comment.
		"quote inside a comment":        {"# say \"\nksail", "# say \"\nksail"},
		"trailing comment with a quote": {"true # it's\nksail", "true # it's\nksail"},
		"hash inside a quoted string":   {"echo \"a # b\nc\"", "echo \"a # b c\""},
		// `#` in the middle of a word is not a comment, so the quote after it is live
		// and the newline inside it folds.
		"hash not starting a word": {"echo a#\"\nb\"", "echo a#\" b\""},
		// After an unquoted continuation the next line continues the word the
		// backslash ended: `echo \` + `# "` is a comment; `foo\` + `#"` is `foo#"`,
		// so that quote is live and folds the newline after it.
		"comment after an unquoted continuation":       {"echo \\\n# say \"\nksail", "echo \\\n# say \"\nksail"},
		"hash continuing a word across a continuation": {"echo foo\\\n#\"\nb\"", "echo foo\\\n#\" b\""},
		// A metacharacter ends the word, so the `#` after it is a comment; inside a
		// word (`foo#bar`) or inside quotes it is not.
		"comment right after a semicolon":            {"true;# say \"\nksail", "true;# say \"\nksail"},
		"hash inside a word after a pipe":            {"a|foo#\"\nb\"", "a|foo#\" b\""},
		"semicolon inside quotes is not a separator": {"echo \"a;#\nb\"", "echo \"a;# b\""},
	}
	for name, c := range cases {
		if got := collapseQuotedNewlines(c[0]); got != c[1] {
			t.Errorf("%s: collapseQuotedNewlines(%q) = %q, want %q", name, c[0], got, c[1])
		}
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

// A NON-LITERAL always-false job condition is skipped by Actions exactly as `if: false`
// is, so a decoy job could supply the only full-framework invocation the validator ever
// saw while the job that actually ran carried a reduced one. This is the reviewer's
// fixture from #3312 verbatim; #3330 tracked it as a residual and it is now closed.
func TestRejectsScanInAnAlwaysFalseComparisonJob(t *testing.T) {
	workflow := "jobs:\n" +
		"  skipped-decoy:\n    if: ${{ 1 == 2 }}\n    steps:\n" +
		"      - run: " + goodScan + "\n" +
		"  reduced-scan:\n    steps:\n" +
		"      - run: env ksail workload scan --framework nsa\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — a job Actions skips cannot supply the framework set; got %q", set)
	}
}

// The `!=` spelling of the same bypass.
func TestRejectsScanInAnAlwaysFalseInequalityJob(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ 'a' != 'a' }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — an always-false inequality job never runs; got %q", set)
	}
}

// CONTROL — an always-TRUE comparison is a job that DOES run, so its scan must still be
// read. Without this the fix could be satisfied by refusing every comparison.
func TestAlwaysTrueComparisonJobIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ 1 == 1 }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("an always-true comparison job runs and must be read, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// CONTROL, and the one that pins the KIND bound. Actions coerces across types, so
// `1 == '1'` evaluates TRUE there and the job RUNS. Treating mismatched kinds as unequal
// would declare this constant-false and reject a legitimate workflow — the expensive
// direction, and the same over-refusal that failed all three real-workflow tests before.
func TestMixedKindComparisonIsNotTreatedAsFalse(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ 1 == '1' }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("a mixed-kind comparison is not decidable here and must stay accepted, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// A BACKSLASH AT THE END OF A SHELL COMMENT DOES NOT CONTINUE THE LINE. Measured in
// bash: the comment ends at the newline and the next line executes as its own command.
// Joining before stripping swallowed that next line into the comment and removed both.
//
// This direction is the FAIL-CLOSED half: the swallowed line is the real scan, so the
// guard saw no invocation at all and refused a workflow that is perfectly fine.
func TestBackslashEndedCommentDoesNotSwallowTheNextLine(t *testing.T) {
	body := "# a comment ending in a backslash \\\n" + goodScan
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected the scan on the line AFTER a backslash-ended comment to be read, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q, want %q", strings.Join(got, ","), "mitre,nsa")
	}
}

// ...and the FAIL-OPEN half, which is why this matters. The swallowed line is a REDUCED
// scan: the guard read only the full-set decoy above it, reported OK, and the workflow
// scanned one framework. Both halves are needed -- fixing only the first could be done
// by never joining at all, which would reopen the continuation hole this guard closed.
func TestBackslashEndedCommentCannotHideAReducedScan(t *testing.T) {
	body := goodScan + "\n# hide the next line \\\nksail workload scan --framework nsa"
	got, err := setOf(t, body)
	if err == nil {
		t.Fatalf("expected FAIL CLOSED: a reduced scan under a backslash-ended comment executes, so it must not be hidden; got %q", got)
	}
}

// CONTROL -- a backslash at the end of a REAL command still continues the line, so the
// reordering must not disable continuation itself. Without this, stripping comments
// first could be "fixed" by dropping the join and reopening devantler-tech/platform#2823.
func TestRealCommandStillContinuesAcrossLines(t *testing.T) {
	body := "ksail workload scan --framework \\\nnsa,mitre --compliance-threshold 95"
	got, err := setOf(t, body)
	if err != nil {
		t.Fatalf("expected a continued REAL command to still be joined, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q, want %q", strings.Join(got, ","), "mitre,nsa")
	}
}

// CONTROL -- a `#` inside a quoted string that SPANS physical lines is content, not a
// comment, and the carried quote state is what keeps it that way. Stripping each
// physical line from a CLEAN state reads the `#` on the continuation line as a comment,
// removes the rest of that line, and takes the reduced scan sitting after the closing
// quote with it -- the same swallowing bug one level down, and fail-open again.
func TestHashInsideAMultilineQuotedStringIsNotAComment(t *testing.T) {
	body := goodScan + "\necho \"opens \\\n# still inside the string\" ; ksail workload scan --framework nsa"
	got, err := setOf(t, body)
	if err == nil {
		t.Fatalf("expected FAIL CLOSED: the reduced scan after the closing quote executes, so a `#` inside the string must not hide it; got %q", got)
	}
}

// A COMPOUND boolean condition is statically decidable when its literal operands
// settle it, and Actions skips such a job exactly as it skips `if: false`. Accepting
// one let a never-running decoy supply the only full-framework invocation the
// validator saw while the job that actually ran carried a reduced one. The reviewer's
// fixture from #3312 verbatim.
func TestRejectsScanInAnAlwaysFalseConjunctionJob(t *testing.T) {
	workflow := "jobs:\n" +
		"  skipped-decoy:\n    if: ${{ false && true }}\n    steps:\n" +
		"      - run: " + goodScan + "\n" +
		"  reduced-scan:\n    steps:\n" +
		"      - run: env ksail workload scan --framework nsa\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — `false && true` never runs; got %q", set)
	}
}

// ACTIONS COMPARES STRINGS CASE-INSENSITIVELY, so `'A' != 'a'` is FALSE there and
// the job never runs. Comparing case-sensitively called it reachable and let the
// decoy in that skipped job satisfy the gate, while only the reduced scan ran.
func TestRejectsScanInACaseInsensitivelyFalseJob(t *testing.T) {
	workflow := "jobs:\n" +
		"  skipped-decoy:\n    if: ${{ 'A' != 'a' }}\n    steps:\n" +
		"      - run: " + goodScan + "\n" +
		"  reduced-scan:\n    steps:\n" +
		"      - run: env ksail workload scan --framework nsa\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — Actions compares strings case-insensitively, so this job never runs; got %q", set)
	}
}

// The MIRROR of the case above, and the reason the fix is EqualFold rather than a
// blanket lowercase: `'A' == 'a'` is TRUE under Actions, so this job DOES run and
// its scan must still be read. Without this, making the comparison case-insensitive
// could have been "reject anything with mixed case", which fails the other way.
func TestCaseInsensitivelyTrueJobIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ 'A' == 'a' }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	set, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("expected the scan in a case-insensitively TRUE job to be read: %v", err)
	}
	if len(set) != 2 {
		t.Fatalf("framework set = %q, want both frameworks", set)
	}
}

// A SCAN WHOSE FAILURE IS DISCARDED IS NOT A GATE. Both of these RUN, so the
// `conditional` test does not catch them -- but a backgrounded command's status
// never reaches the step, and without `pipefail` only the last pipeline stage's
// does. Measured: the backgrounded decoy exited 42 and the step still succeeded,
// so the full framework list was credited to a command that gated nothing.
func TestRejectsScanWhoseStatusIsMasked(t *testing.T) {
	cases := map[string]string{
		"backgrounded with &": goodScan + " &\n" +
			"env ksail workload scan --framework nsa",
		"piped into another command": goodScan + " | tee /dev/null\n" +
			"env ksail workload scan --framework nsa",
	}
	for name, body := range cases {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a scan whose exit status is discarded is not a gate", name)
		}
	}
}

// The MIRROR: `&&` and `||` still reach the CONDITIONAL refusal, not the new one,
// and an ordinary unconditional scan is still accepted. Without this, "refuse
// anything containing & or |" would look identical on the cases above while
// breaking every real workflow.
func TestPlainScanIsStillAcceptedAlongsideStatusMasking(t *testing.T) {
	set, err := setOf(t, goodScan)
	if err != nil {
		t.Fatalf("a plain unconditional scan must still be read: %v", err)
	}
	if len(set) != 2 {
		t.Fatalf("framework set = %q, want both frameworks", set)
	}
}

// A `run:` SCALAR IS INPUT TO THE STEP'S SHELL, NOT NECESSARILY A BASH PROGRAM.
// Under a custom `command {0}` template Actions writes the script to a file and runs
// the template, so `shell: cat {0}` PRINTS the decoy and exits 0 without executing it.
// The three cases cover the step's own `shell:`, the job default, and the workflow
// default, because resolving only the first leaves the other two open.
func TestRejectsScanUnderANonExecutingShell(t *testing.T) {
	cases := map[string]string{
		"step shell": "jobs:\n  validate:\n    steps:\n" +
			"      - run: " + goodScan + "\n        shell: cat {0}\n" +
			"      - run: env ksail workload scan --framework nsa\n",
		"job default shell": "jobs:\n  validate:\n    defaults:\n      run:\n        shell: cat {0}\n    steps:\n" +
			"      - run: " + goodScan + "\n",
		"workflow default shell": "defaults:\n  run:\n    shell: cat {0}\njobs:\n  validate:\n    steps:\n" +
			"      - run: " + goodScan + "\n",
	}
	for name, workflow := range cases {
		path := writeTemp(t, workflow)
		if set, err := frameworkSet(path); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a non-executing shell runs no scan; got %q", name, set)
		}
	}
}

// The MIRROR: an EXPLICIT bash/sh shell still executes, so its scan is still read.
// Without this, the fix could have been "refuse any declared shell", which would
// reject legitimate workflows.
func TestScanUnderAnExplicitBashShellIsStillRead(t *testing.T) {
	for _, sh := range []string{"bash", "sh"} {
		workflow := "jobs:\n  validate:\n    steps:\n" +
			"      - run: " + goodScan + "\n        shell: " + sh + "\n"
		path := writeTemp(t, workflow)
		set, err := frameworkSet(path)
		if err != nil {
			t.Fatalf("shell %s: expected the scan to be read: %v", sh, err)
		}
		if len(set) != 2 {
			t.Fatalf("shell %s: framework set = %q, want both frameworks", sh, set)
		}
	}
}

// TEXT AFTER A CLOSING MULTILINE QUOTE IS ARGUMENT TEXT, NOT A NEW COMMAND.
// Measured in bash: `printf %s 'a` / `b' ksail workload scan ...` printed those words
// as printf ARGUMENTS and never invoked ksail, while the guard read the remainder as
// a standalone command and credited the full framework list to it.
func TestRejectsScanInAMultilineQuoteRemainder(t *testing.T) {
	body := "printf %s 'text\nmore' " + goodScan + "\n" +
		"env ksail workload scan --framework nsa"
	if set, err := setOf(t, body); err == nil {
		t.Fatalf("expected FAIL CLOSED — the scan is an argument to printf, not a command; got %q", set)
	}
}

// The MIRROR, and it caught a real over-rejection while this was being written:
// an OPERATOR after the closing quote does start a new command, so `'a` / `b'; ksail`
// really does run the scan -- confirmed against bash. Keying only on "a quote just
// closed" refused this legitimate invocation.
func TestScanAfterAnOperatorInAQuoteRemainderIsStillRead(t *testing.T) {
	set, err := setOf(t, "printf %s 'text\nmore'; "+goodScan)
	if err != nil {
		t.Fatalf("a scan after `;` in a quote remainder is a real command: %v", err)
	}
	if len(set) != 2 {
		t.Fatalf("framework set = %q, want both frameworks", set)
	}
}

// The `||` spelling: both branches false, so the job never runs.
func TestRejectsScanInAnAlwaysFalseDisjunctionJob(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ false || false }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — `false || false` never runs; got %q", set)
	}
}

// Negation of a literal is decidable too.
func TestRejectsScanInANegatedTrueJob(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ !true }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — `!true` never runs; got %q", set)
	}
}

// Parentheses group, and a decidable comparison inside one still settles the whole.
func TestRejectsScanInAParenthesisedAlwaysFalseJob(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ (1 == 2) && true }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	if set, err := frameworkSet(path); err == nil {
		t.Fatalf("expected FAIL CLOSED — `(1 == 2) && true` never runs; got %q", set)
	}
}

// CONTROL — an always-TRUE conjunction is a job that RUNS, so its scan must still be
// read. Without this the fix could be satisfied by refusing every compound condition.
func TestAlwaysTrueConjunctionJobIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ true && true }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("an always-true conjunction job runs and must be read, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// CONTROL — `false || true` is TRUE, so the job runs. Short-circuiting the wrong way
// here would refuse a legitimate workflow.
func TestDisjunctionWithATrueBranchIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ false || true }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("`false || true` runs and must be read, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// CONTROL — negating a false literal yields a job that runs.
func TestNegatedFalseJobIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ !false }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("`!false` runs and must be read, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// CONTROL, and the bound that keeps the REAL workflow valid: a context reference is not
// decidable here, so a compound containing one stays ACCEPTED. Refusing undecidable
// conditions was measured to fail all three real-workflow tests.
func TestUndecidableConjunctionIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ github.event_name == 'push' && true }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("an undecidable conjunction must stay accepted, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// CONTROL — the shipped `validate` job's own shape, disjoined with a literal.
func TestUndecidableDisjunctionIsStillRead(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ needs.changes.outputs.k8s == 'true' || false }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("an undecidable disjunction must stay accepted, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// CONTROL — an operator INSIDE a string literal is content, not structure. Splitting
// without quote awareness would tear this comparison apart.
func TestOperatorInsideAStringLiteralIsContent(t *testing.T) {
	workflow := "jobs:\n  validate:\n    if: ${{ 'a&&b' == 'a&&b' }}\n    steps:\n" +
		"      - run: " + goodScan + "\n"
	path := writeTemp(t, workflow)
	got, err := frameworkSet(path)
	if err != nil {
		t.Fatalf("an equal same-kind comparison is TRUE and must be read, got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("set = %q", got)
	}
}

// `||` DISCARDS THE FAILURE OF THE COMMAND ON ITS LEFT, so a scan spelled there is not a
// gate even though nothing precedes it and no single `&` or `|` ends it. Under the default
// `bash -e` a non-zero left side of `||` is not an error at all -- the right side runs and
// its status becomes the step's -- so `scan --framework nsa,mitre || true` reports the full
// framework list while a later reduced scan actually decides the outcome. This is the same
// status-gating defect the `&`/`|` cases cover, reached by the one operator that marks what
// FOLLOWS it conditional and so was never tested against what PRECEDES it.
func TestRejectsScanWhoseFailureIsSwallowedByOrElse(t *testing.T) {
	cases := map[string]string{
		"|| true hides an env-wrapped reduced scan": goodScan + " || true\n" +
			"env ksail workload scan --framework nsa",
		"|| true alone":     goodScan + " || true",
		"|| echo":           goodScan + " || echo scan failed",
		"|| a variable":     goodScan + " || $FALLBACK",
		"|| exit 0":         goodScan + " || exit 0",
		"|| nothing at all": goodScan + " ||",
		// The re-raise must itself reach the step: `&` backgrounds the `exit 1`.
		"|| exit 1 backgrounded": goodScan + " || exit 1 &",
		// A shell exit code is taken modulo 256, so these two leave status 0 and mask
		// the failure while READING like a re-raise.
		"|| exit 256":  goodScan + " || exit 256",
		"|| exit -256": goodScan + " || exit -256",
		"|| exit 512":  goodScan + " || exit 512",
	}
	for name, body := range cases {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a scan whose failure `||` swallows is not a gate", name)
		}
	}
}

// The other half of the whitelist: a right side that is CERTAIN to fail re-raises the
// scan's failure, so those two spellings stay readable. `|| exit 1` is already covered by
// TestUnconditionalScanWithTrailingOperatorIsAccepted; this pins `|| false` and a
// non-1 exit code so the rule is "the code is non-zero", not "the code is 1".
func TestScanIsStillReadWhenOrElseReRaisesTheFailure(t *testing.T) {
	cases := map[string]string{
		"|| false":  goodScan + " || false",
		"|| exit 2": goodScan + " || exit 2",
		// -1 normalises to 255, which IS a failure -- so the rule is the normalised
		// status, not "the literal is positive".
		"|| exit -1": goodScan + " || exit -1",
	}
	for name, body := range cases {
		set, err := setOf(t, body)
		if err != nil {
			t.Fatalf("%s: the right side re-raises the failure, so the scan must still be read: %v", name, err)
		}
		if len(set) != 2 {
			t.Fatalf("%s: framework set = %q, want both frameworks", name, set)
		}
	}
}

// The MIRROR, and the reason this is not "refuse any doubled operator": `&&` PRESERVES the
// left side's failure -- if the scan fails the right side never runs and its status is the
// step's -- so a scan followed by `&&` must still be read. Without this control, masking
// every doubled operator would pass the case above while refusing a legitimate gate.
func TestScanFollowedByAndAndIsStillRead(t *testing.T) {
	set, err := setOf(t, goodScan+" && echo scanned")
	if err != nil {
		t.Fatalf("`&&` preserves the scan's failure, so it must still be read: %v", err)
	}
	if len(set) != 2 {
		t.Fatalf("framework set = %q, want both frameworks", set)
	}
}

// --- The COMMAND-PREFIX class ------------------------------------------------
//
// The shape test requires the segment's FIRST three tokens to be
// `ksail workload scan`, so a prefixed invocation — `env ksail ...`, and by the
// same token filter any wrapper such as `sudo`, `nice` or `time` — is not counted.
//
// ALONE that fails closed: the scalar has no countable invocation and the guard
// refuses. The defect is in COMBINATION. When a counted invocation and a prefixed
// one coexist, the guard validates the counted one and silently ignores the one
// that ALSO runs, so the executed framework set need not be the validated set.
// That is what makes the decoy bypass in #3312 work.
//
// Refused rather than read, matching every other unreadable form in this guard:
// the prefix may set the environment the scan runs in, so its meaning is not
// decidable from the text alone. See #3338.
func TestRejectsPrefixedScanInvocation(t *testing.T) {
	cases := map[string]string{
		"counted invocation paired with an env-prefixed executing scan": goodScan + " -o kubescape.sarif\n" +
			"env ksail workload scan --framework nsa -o kubescape.sarif\n",
		"counted invocation paired with an env-assignment-prefixed executing scan": goodScan + " -o kubescape.sarif\n" +
			"env FOO=1 ksail workload scan --framework nsa -o kubescape.sarif\n",
		"env-prefixed scan alone":            "env ksail workload scan --framework nsa -o kubescape.sarif\n",
		"env-assignment-prefixed scan alone": "env FOO=1 ksail workload scan --framework nsa -o kubescape.sarif\n",
		// QUOTING THE COMMAND NAME STILL RUNS IT. `shellSplit` preserves quote
		// characters, so `'ksail'` is one token that an exact match on `ksail` misses
		// — the same bypass this guard exists to close, one spelling further out.
		"single-quoted ksail token in an env-prefixed scan": goodScan + " -o kubescape.sarif\n" +
			"env 'ksail' workload scan --framework nsa -o kubescape.sarif\n",
		"double-quoted ksail token in an env-prefixed scan": goodScan + " -o kubescape.sarif\n" +
			"env \"ksail\" workload scan --framework nsa -o kubescape.sarif\n",
		// CONCATENATION AND ESCAPES RESOLVE TO `ksail` TOO, and a token-equality test
		// that only strips a WRAPPING quote pair misses every one of them. To the shell
		// `k"s"ail`, `k's'ail` and `k\sail` are all the word `ksail`, so each executes
		// the scan while reading as an unrelated token — the same bypass as the quoted
		// spellings above, one concatenation further out.
		"concatenated double-quoted ksail token in an env-prefixed scan": goodScan + " -o kubescape.sarif\n" +
			"env k\"s\"ail workload scan --framework nsa -o kubescape.sarif\n",
		"concatenated single-quoted ksail token in an env-prefixed scan": goodScan + " -o kubescape.sarif\n" +
			"env k's'ail workload scan --framework nsa -o kubescape.sarif\n",
		"backslash-escaped ksail token in an env-prefixed scan": goodScan + " -o kubescape.sarif\n" +
			"env k\\sail workload scan --framework nsa -o kubescape.sarif\n",
	}
	for name, body := range cases {
		if _, err := setOf(t, body); err == nil {
			t.Fatalf("%s: expected FAIL CLOSED — a prefixed scan invocation executes but is not "+
				"validated, so the executed framework set need not be the validated one", name)
		}
	}
}

// A QUOTED SPELLING IN COMMAND POSITION EXECUTES TOO. `bareToken` collapses
// `k"s"ail`, `k's'ail`, `k\sail` and `'ksail'` to the word `ksail` for the
// prefixed-scan sweep, but that sweep starts at field index 1, so the same
// spelling at field index 0 met a RAW comparison: neither counted as an
// invocation nor refused as a prefixed one, it fell through as ordinary shell.
// Paired with a counted invocation the guard validated the counted one and
// ignored the one that also runs — the exact bypass #3338 closes, at the one
// position the fix did not cover. Measured: every case below PASSED against the
// unnormalised comparison.
//
// The honest reading is that these ARE invocations, so the shape assertions are
// two-sided: paired with a counted scan they are a second counted invocation
// (refused, because the guard cannot tell which produces the uploaded SARIF), and
// ALONE with the full set they are the gate itself and must be read.
func TestQuotedScanSpellingInCommandPositionIsCounted(t *testing.T) {
	spellings := map[string]string{
		"concatenated double-quoted": "k\"s\"ail",
		"concatenated single-quoted": "k's'ail",
		"backslash-escaped":          "k\\sail",
		"single-quoted":              "'ksail'",
		"double-quoted":              "\"ksail\"",
	}
	for name, word := range spellings {
		paired := goodScan + " -o kubescape.sarif\n" +
			word + " workload scan --framework nsa -o kubescape.sarif\n"
		_, err := setOf(t, paired)
		if err == nil {
			t.Errorf("%s ksail paired with a counted scan: expected FAIL CLOSED — the spelling "+
				"executes a second scan, so the validated framework set need not be the one "+
				"that runs", name)
			continue
		}
		// ASSERT A SPACED PHRASE, NEVER A BARE WORD (see the note in
		// TestUnquotedEchoedScanTextIsRefusedAndNamesTheQuotingRemedy): the fixture
		// path embeds this test's name, so a bare token could match the path.
		if !strings.Contains(err.Error(), "scan invocations") {
			t.Errorf("%s ksail paired with a counted scan: the refusal must be the two-invocation "+
				"one, proving the spelling was COUNTED rather than refused as prefixed text; got: %v",
				name, err)
			continue
		}

		alone := word + " workload scan --framework nsa,mitre --compliance-threshold 95"
		got, err := setOf(t, alone)
		if err != nil {
			t.Errorf("%s ksail alone: expected ACCEPT — it is the gate, spelled differently; got: %v",
				name, err)
			continue
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Errorf("%s ksail alone: normalised set = %q, want %q", name, strings.Join(got, ","), "mitre,nsa")
		}
	}
}

// THE COMMAND WORD IS DECIDABLE ONLY WHEN IT IS PLAIN TEXT. `bareToken` resolves
// quotes and backslashes, and that is the whole decidable set: an ANSI-C quote
// (`k$'s'ail`), a locale quote (`k$"s"ail`), a variable (`$KSAIL`), a substitution
// or a backtick all produce their command word only when the shell runs, so no
// lexical test can tell `k$'s'ail workload scan` — which executes ksail — from an
// unrelated word. Enumerating expansion syntaxes is the blacklist this guard
// exists to avoid, so the rule is inverted into a WHITELIST: on a line that
// carries `--framework`, every token before it must be free of `$` and backticks,
// and the word in front of `workload scan` must resolve to exactly `ksail` or be a
// quoted-string fragment. Anything else is refused as undecidable. Measured: every
// paired case below PASSED against the quote-only normaliser.
func TestRejectsUndecidableScanCommandWord(t *testing.T) {
	cases := map[string]string{
		"ANSI-C quoted ksail in command position":          "k$'s'ail workload scan --framework nsa -o kubescape.sarif\n",
		"ANSI-C quoted ksail in a prefixed scan":           "env k$'s'ail workload scan --framework nsa -o kubescape.sarif\n",
		"locale quoted ksail in command position":          "k$\"s\"ail workload scan --framework nsa -o kubescape.sarif\n",
		"variable in command position":                     "$KSAIL workload scan --framework nsa -o kubescape.sarif\n",
		"ANSI-C quote inside the workload token":           "ksail w$'o'rkload scan --framework nsa -o kubescape.sarif\n",
		"closed backtick substitution in command position": "`printf ksail` workload scan --framework nsa -o kubescape.sarif\n",
		"path-qualified ksail in command position":         "/usr/local/bin/ksail workload scan --framework nsa -o kubescape.sarif\n",
		"relative-path ksail in command position":          "./ksail workload scan --framework nsa -o kubescape.sarif\n",
	}
	for name, line := range cases {
		for _, shape := range []struct {
			label string
			body  string
		}{
			{"paired with a counted scan", goodScan + " -o kubescape.sarif\n" + line},
			{"alone", line},
		} {
			_, err := setOf(t, shape.body)
			if err == nil {
				t.Errorf("%s, %s: expected FAIL CLOSED — the command word is not decidable from "+
					"the text, so the line may execute a scan the guard never validated", name, shape.label)
				continue
			}
			// ASSERT A SPACED PHRASE, NEVER A BARE WORD (see
			// TestUnquotedEchoedScanTextIsRefusedAndNamesTheQuotingRemedy).
			if !strings.Contains(err.Error(), "not decidable from the text") {
				t.Errorf("%s, %s: the refusal must name undecidability, proving this rule fired "+
					"rather than a coincidental one; got: %v", name, shape.label, err)
			}
		}
	}
}

// The control for the whitelist above: an expansion AFTER `--framework` is an
// ordinary argument, and the real workflow has one. Refusing it would fail the
// known-good configuration on the rule's first run, which is how a control gets
// switched off.
func TestExpansionInScanArgumentsIsStillRead(t *testing.T) {
	got, err := setOf(t, goodScan+" --format sarif -o \"${RUNNER_TEMP}/kubescape.sarif\"\n")
	if err != nil {
		t.Fatalf("expected ACCEPT — an expansion in an argument does not change the command word; got: %v", err)
	}
	if strings.Join(got, ",") != "mitre,nsa" {
		t.Fatalf("normalised set = %q, want %q", strings.Join(got, ","), "mitre,nsa")
	}
}

// The second control: `--framework` alone is too weak a trigger. An unrelated
// command whose quoted text happens to carry `$` and `--framework` invokes no scan,
// so refusing it would tax every tool that takes a `--framework` flag. The
// whitelist therefore fires only when a scan word stands before `--framework`.
func TestUnrelatedTextWithExpansionAndFrameworkIsStillIgnored(t *testing.T) {
	cases := map[string]string{
		"quoted expansion before --framework in an echo": goodScan + "\necho '$HOME --framework'\n",
		"other tool taking --framework after a variable": goodScan + "\nsome-tool --config $CFG --framework x\n",
	}
	for name, body := range cases {
		got, err := setOf(t, body)
		if err != nil {
			t.Fatalf("%s: expected ACCEPT — no scan word precedes --framework, so nothing here can invoke the scan; got: %v", name, err)
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Fatalf("%s: normalised set = %q, want %q", name, strings.Join(got, ","), "mitre,nsa")
		}
	}
}

// The control for the rejection above, and the reason it keys on the three scan
// tokens rather than on the prefix word. An `env` step that is not a scan is
// ordinary shell and must still be ignored, or "reject any line containing `env`"
// would pass the test above while breaking legitimate workflows.
func TestUnrelatedPrefixedCommandIsStillIgnored(t *testing.T) {
	cases := map[string]string{
		"env-assignment step beside a scan": goodScan + "\nenv FOO=1 echo hello\n",
		"env step beside a scan":            goodScan + "\nenv printenv HOME\n",
		"wrapper on an unrelated command":   goodScan + "\ntime ls -la\n",
		// A multi-word quoted string arrives as tokens whose quotes are UNBALANCED
		// (`'ksail` … `nsa'`), so it is echoed text rather than an execution and must
		// stay accepted. This is the control for unquoting only fully wrapped tokens.
		"echoed scan text in a quoted string": goodScan + "\necho 'ksail workload scan --framework nsa'\n",
		// A leading word inside the quotes used to make the interior `ksail workload
		// scan` read as a PREFIXED invocation once the string was split at spaces.
		"echoed scan text behind a leading word": goodScan + "\necho \"about ksail workload scan --framework nsa\"\n",
	}
	for name, body := range cases {
		got, err := setOf(t, body)
		if err != nil {
			t.Fatalf("%s: expected ACCEPT — an unrelated prefixed command is not a scan, got: %v", name, err)
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Fatalf("%s: normalised set = %q, want %q", name, strings.Join(got, ","), "mitre,nsa")
		}
	}
}

// UNQUOTED TEXT THAT MERELY CONTAINS THE SCAN TOKENS IS REFUSED, AND THAT IS THE
// DECISION RATHER THAN AN OVERSIGHT. `env ksail workload scan` (executes) and
// `echo ksail workload scan` (prints) are TOKEN-IDENTICAL in shape; only knowing
// which commands execute their arguments separates them, so no lexical test can
// tell them apart. Accepting the echo form therefore means enumerating the
// commands that DO execute — `env`, `sudo`, `nice`, `time`, `xargs`, `command`,
// `exec`, `nohup`, `timeout`, `setsid`, `stdbuf`, `chroot`, `doas`, … — which is
// exactly the prefix blacklist this guard was built to avoid, and the spelling it
// missed would be the bypass. Refused rather than read, like every other
// undecidable form here.
//
// The author's opt-out is one character per side: QUOTE the text, which makes it
// unambiguously an argument rather than an invocation. That form is accepted and
// pinned as the control in TestUnrelatedPrefixedCommandIsStillIgnored, so the
// error message MUST name it — advising "invoke the scan with no prefix" to
// someone who was never invoking a scan sends them to fix the wrong thing.
func TestUnquotedEchoedScanTextIsRefusedAndNamesTheQuotingRemedy(t *testing.T) {
	body := goodScan + "\necho ksail workload scan --framework nsa\n"

	_, err := setOf(t, body)
	if err == nil {
		t.Fatalf("expected FAIL CLOSED — an unquoted token sequence is indistinguishable " +
			"from a wrapper-prefixed invocation, so it cannot be accepted without " +
			"enumerating the commands that execute their arguments")
	}
	// ASSERT A SPACED PHRASE, NEVER A BARE WORD. `setOf` writes its fixture under
	// `t.TempDir()`, whose path embeds the TEST NAME, and the error is prefixed with
	// that path — so a bare `Contains(err, "quote")` is satisfied by the "Unquoted"
	// in this test's own name and passes no matter what the message says. Measured:
	// it passed against the un-fixed message. A phrase containing a space cannot
	// occur in that path.
	if !strings.Contains(err.Error(), "quote the text") {
		t.Fatalf("the error must name the quoting opt-out, or text that never invoked a "+
			"scan is sent to fix a prefix it does not have; got: %v", err)
	}
}

// The option-word half of the whitelist. The command-word rule stops at `--framework`,
// and the option word itself can be constructed by the shell: `--frame${SUFFIX}` carries
// no literal `--framework`, so the primary-scan path used to skip it as an unframed scan
// — neither counted nor refused — while it executed exactly the option the guard never
// read. Paired with a counted invocation, the validated set need not be the executed one.
// A bare expansion AFTER the flag is the same hole from the other side: `$EXTRA` can
// expand to a second `--framework` that overrides the one the guard read.
func TestRejectsUndecidableScanOptionWord(t *testing.T) {
	cases := map[string]string{
		"variable suffix builds the option word":                      "ksail workload scan --frame${SUFFIX} nsa\n",
		"ANSI-C fragment builds the option word":                      "ksail workload scan --frame$'work' nsa\n",
		"quoted fragment builds the option word":                      "ksail workload scan --frame\"work\" nsa\n",
		"backslash escape builds the option word":                     "ksail workload scan --frame\\work nsa\n",
		"bare expansion after the flag":                               "ksail workload scan --framework nsa $EXTRA\n",
		"quoted expansion in option position":                         "ksail workload scan --framework nsa \"$FLAG\" mitre\n",
		"expansion inside an --framework= value":                      "ksail workload scan --framework=$FW\n",
		"expansion in an option word after the flag":                  "ksail workload scan --framework nsa --$OPT sarif\n",
		"prefixed scan with a constructed option word":                "env ksail workload scan --frame${SUFFIX} nsa\n",
		"prefixed scan with a bare expansion":                         "env KUBECONFIG=x ksail workload scan $EXTRA\n",
		"unframed primary scan with a bare expansion":                 "ksail workload scan $ARGS\n",
		"undecidable command word with a constructed option word":     "k$'s'ail workload scan --frame${SUFFIX} nsa\n",
		"prefixed undecidable command word with a constructed option": "env k$'s'ail workload scan --frame${SUFFIX} nsa\n",
		"variable command word with a bare expansion":                 "$KSAIL workload scan $ARGS\n",
		"prefix argument masks the flag search":                       "env NOTE=--framework k$'s'ail workload scan --framework nsa\n",
		"quoted expansion after a value-carrying option":              "ksail workload scan --framework=nsa,mitre \"$EXTRA\"\n",
		"quoted expansion after a boolean-looking option":             "ksail workload scan --framework nsa --verbose \"$X\"\n",
		"glob in an option word":                                      "ksail workload scan --framewor? nsa -o kubescape.sarif\n",
		"bracket pattern in an option word":                           "ksail workload scan --framewor[k] nsa\n",
		"glob in an unquoted argument":                                "ksail workload scan --framework nsa -o out*.sarif\n",
	}
	for name, line := range cases {
		for _, shape := range []struct {
			label string
			body  string
		}{
			{"paired with a counted scan", goodScan + " -o kubescape.sarif\n" + line},
			{"alone", line},
		} {
			_, err := setOf(t, shape.body)
			if err == nil {
				t.Errorf("%s, %s: expected FAIL CLOSED — an option word or argument is not decidable "+
					"from the text, so the line may execute a framework set the guard never validated", name, shape.label)
				continue
			}
			// ASSERT A SPACED PHRASE, NEVER A BARE WORD (see
			// TestUnquotedEchoedScanTextIsRefusedAndNamesTheQuotingRemedy).
			if !strings.Contains(err.Error(), "not decidable from the text") {
				t.Errorf("%s, %s: the refusal must name undecidability, proving this rule fired "+
					"rather than a coincidental one; got: %v", name, shape.label, err)
			}
		}
	}
}

// The control for the option-word rule: the ONE accepted expansion shape is a
// double-quoted value of a plain option word, which is how the real workflow writes its
// output path. A plain quoted argument (`'nsa'`) is decidable too — it cannot construct
// an option word — and is still refused by frameworkTokens as a value, for its own reason.
func TestQuotedOptionValueExpansionIsStillRead(t *testing.T) {
	for name, body := range map[string]string{
		"double-quoted value of -o":       goodScan + " -o \"${RUNNER_TEMP}/kubescape.sarif\"\n",
		"double-quoted value of --output": goodScan + " --format sarif --output \"$OUT\"\n",
		"double-quoted glob value of -o":  goodScan + " -o \"out*.sarif\"\n",
		// Single quotes and backslashes make these literal file names, not expansions:
		// the shell passes `*.sarif` and `out*.sarif` through unchanged.
		"single-quoted glob value of -o":          goodScan + " -o '*.sarif'\n",
		"backslash-escaped glob value of -o":      goodScan + " -o out\\*.sarif\n",
		"single-quoted dollar value of -o":        goodScan + " -o '$OUT'\n",
		"single-quoted bracket value of --output": goodScan + " --format sarif --output '[a].sarif'\n",
		// A quoted value containing a space is still ONE shell argument; splitting it at
		// the space used to leave neither half a whole double-quoted word.
		"double-quoted value of -o with a space": goodScan + " -o \"${RUNNER_TEMP}/kubescape report.sarif\"\n",
	} {
		got, err := setOf(t, body)
		if err != nil {
			t.Fatalf("%s: expected ACCEPT — a double-quoted value of a plain option word cannot construct an option; got: %v", name, err)
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Fatalf("%s: normalised set = %q, want %q", name, strings.Join(got, ","), "mitre,nsa")
		}
	}
	_, err := setOf(t, "ksail workload scan --framework 'nsa,mitre'\n")
	if err == nil || strings.Contains(err.Error(), "not decidable from the text") {
		t.Fatalf("a quoted framework VALUE is decidable (it cannot construct an option word) and must be refused by the value check instead; got: %v", err)
	}
}

// Both anchors constructed away at once. The command-word rule fires only behind a
// literal `--framework`, and the option-word rule only behind a resolvable `workload
// scan` pair — so a line that constructs one scan word AND the option word (`ksail
// work${LOAD} scan --frame${SUFFIX} nsa`) carried neither anchor and fell between the two
// branches: not counted, not refused, executing a scan the guard never read. The
// whitelist is now keyed on the one plain scan word such a line still needs, so any
// expansion beside it is refused whatever the rest spells.
func TestRejectsConstructedScanWordBesideAConstructedOptionWord(t *testing.T) {
	cases := map[string]string{
		"constructed workload and option word":                     "ksail work${LOAD} scan --frame${SUFFIX} nsa\n",
		"constructed scan and option word":                         "ksail workload sc${AN} --frame${SUFFIX} nsa\n",
		"constructed ksail and scan with a constructed option":     "$KSAIL workload sc${AN} --frame${SUFFIX} nsa\n",
		"constructed ksail and workload with a constructed option": "$KSAIL work${LOAD} scan --frame${SUFFIX} nsa\n",
		"ANSI-C fragment in workload beside a constructed option":  "ksail w$'o'rkload scan --frame${SUFFIX} nsa\n",
		"glob in the scan word beside a constructed option":        "ksail workload sca? --frame${SUFFIX} nsa\n",
		// Quoting that does NOT neutralise the expansion: `$` inside double quotes and
		// a bare `$` after a quoted scan word both still expand at run time.
		"double-quoted expansion beside a quoted scan word":              "'ksail' \"work${LOAD}\" scan --frame${SUFFIX} nsa\n",
		"unquoted expansion beside a single-quoted scan word":            "'ksail' work${LOAD} scan --framework nsa\n",
		"prefixed, with workload and the option word constructed":        "env ksail work${LOAD} scan --frame${SUFFIX} nsa\n",
		"assignment-prefixed, with scan and the option word constructed": "LOAD=load ksail workload sc${AN} --frame${SUFFIX} nsa\n",
	}
	for name, line := range cases {
		for _, shape := range []struct {
			label string
			body  string
		}{
			{"paired with a counted scan", goodScan + " -o kubescape.sarif\n" + line},
			{"alone", line},
		} {
			_, err := setOf(t, shape.body)
			if err == nil {
				t.Errorf("%s, %s: expected FAIL CLOSED — a plain scan word stands beside a shell "+
					"expansion, so the line may execute a scan the guard never validated", name, shape.label)
				continue
			}
			// ASSERT A SPACED PHRASE, NEVER A BARE WORD (see
			// TestUnquotedEchoedScanTextIsRefusedAndNamesTheQuotingRemedy).
			if !strings.Contains(err.Error(), "not decidable from the text") {
				t.Errorf("%s, %s: the refusal must name undecidability, proving this rule fired "+
					"rather than a coincidental one; got: %v", name, shape.label, err)
			}
		}
	}
}

// The control: a plain scan word beside NO expansion, and an expansion beside NO plain
// scan word, are both still ordinary shell. Quoted prose that merely contains the words
// arrives as unbalanced fragments, which are not plain words.
func TestPlainScanWordWithoutAnExpansionIsStillIgnored(t *testing.T) {
	cases := map[string]string{
		"quoted prose naming the scan beside a variable": goodScan + "\necho \"ksail workload scan finished: $STATUS\"\n",
		"scan word in unrelated plain text":              goodScan + "\necho scan finished\n",
		"expansion with no scan word":                    goodScan + "\nsome-tool --config $CFG --output x\n",
		// Single quotes make the characters literal, so none of these is an expansion:
		// the quoted scan word beside them is a plain word, and the line is still prose.
		"single-quoted dollar beside a quoted scan word":   goodScan + "\necho 'scan' '$HOME'\n",
		"single-quoted glob beside a quoted scan word":     goodScan + "\necho 'ksail' '*.yaml'\n",
		"single-quoted wildcard beside a quoted scan word": goodScan + "\necho 'workload' 'ksail?'\n",
		"single-quoted bracket beside a quoted scan word":  goodScan + "\necho 'scan' '[abc]'\n",
		"escaped dollar beside a plain scan word":          goodScan + "\necho scan \\$HOME\n",
	}
	for name, body := range cases {
		got, err := setOf(t, body)
		if err != nil {
			t.Fatalf("%s: expected ACCEPT — nothing here pairs a plain scan word with an expansion; got: %v", name, err)
		}
		if strings.Join(got, ",") != "mitre,nsa" {
			t.Fatalf("%s: normalised set = %q, want %q", name, strings.Join(got, ","), "mitre,nsa")
		}
	}
}
