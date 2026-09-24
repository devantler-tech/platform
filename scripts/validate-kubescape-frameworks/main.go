// Assert the Kubescape posture gate still evaluates every framework the
// exception set depends on.
//
// WHY THIS EXISTS (#2823)
// The gate scanned `nsa` alone for a long time. The ClusterSecurityException CRs
// name 76 distinct controls; NSA-CISA evaluates 17 of them, so 59 excepted
// controls — including every RBAC control those CRs exist to govern — were never
// scored, never gated, and never sent to Code Scanning. Nothing failed. The score
// simply did not include them.
//
// That is the failure mode this guard exists for: dropping a framework REMOVES
// findings, so the compliance score goes UP and every check stays green. A
// coverage regression here is indistinguishable from an improvement unless
// something asserts the framework list itself.
//
// WHY THIS IS STRUCTURAL RATHER THAN TEXTUAL (#3060)
// The predecessor matched raw file text and subtracted known decoys. That is
// unbounded: each round closed one spelling and left the class open. Requiring
// `ksail` as a line's first token closed the command-SHAPE class, but not the
// shell-CONTEXT one — a heredoc BODY line genuinely begins with `ksail` while
// executing nothing, so a decoy heredoc could supply the framework list the
// guard read while the real scan ran elsewhere in a form the matcher skipped.
//
// This reads only what actually executes: `run:` scalars taken from the parsed
// workflow, then lines of those scalars with heredoc bodies removed. Both axes
// are closed by construction rather than by enumeration.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// BOTH workflows, and that is the point rather than thoroughness. They upload
// under the SAME Code Scanning category, so validate-main.yaml's run is the
// durable main-branch baseline that ci.yaml's PR alerts are diffed against. If
// the two scan different frameworks, findings only the PR sees never persist as
// a main-branch alert, and a direct push to main — which bypasses the merge
// queue — goes ungated on whatever the baseline omits.
var defaultWorkflows = []string{
	".github/workflows/ci.yaml",
	".github/workflows/validate-main.yaml",
}

// Every framework the gate must evaluate. `mitre` is here because it is the only
// framework that reaches C-0007, C-0015, C-0031, C-0037, C-0045, C-0048 and
// C-0053 — the excepted RBAC controls. Removing it silently un-gates all seven.
var requiredFrameworks = []string{"nsa", "mitre"}

// A framework name is a plain token. Anything else — a variable, an expression,
// a quoted string — fails closed rather than being truncated to whatever prefix
// happens to match.
var frameworkToken = regexp.MustCompile(`^[a-z0-9._-]+$`)

// Only this literal subset can be split on whitespace without evaluating shell
// syntax. Complex command strings keep the conservative substring rule below.
var literalShellWords = regexp.MustCompile(`^[a-zA-Z0-9_./[:space:]-]+$`)

func main() {
	workflows := os.Args[1:]
	if len(workflows) == 0 {
		root, err := repoRoot()
		if err != nil {
			fatal("%v", err)
		}
		for _, w := range defaultWorkflows {
			workflows = append(workflows, filepath.Join(root, w))
		}
	}

	// Fewer than two workflows cannot express the cross-workflow equality that
	// is half of this guard's purpose, so it is an error rather than a partial
	// check that reports success.
	if len(workflows) < 2 {
		fatal("at least two workflows are required; got %d. The main baseline must be compared against the PR gate. See #2823.", len(workflows))
	}

	sets := make([]string, 0, len(workflows))
	failed := false
	for _, w := range workflows {
		set, err := frameworkSet(w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "::error::%v\n", err)
			failed = true
			sets = append(sets, "")
			continue
		}
		if err := checkRequired(set); err != nil {
			fmt.Fprintf(os.Stderr, "::error file=%s::%v\n", w, err)
			failed = true
		}
		sets = append(sets, strings.Join(set, ","))
	}

	// The required members are a FLOOR; the workflows must also agree EXACTLY.
	// Checking membership alone accepted `nsa,mitre,pss` against an `nsa,mitre`
	// baseline.
	if !failed {
		for i := 1; i < len(sets); i++ {
			if sets[i] != sets[0] {
				fmt.Fprintf(os.Stderr, "::error::framework sets differ: %s has [%s] but %s has [%s].\n",
					workflows[0], sets[0], workflows[i], sets[i])
				fmt.Fprintf(os.Stderr, "::error::Both upload to one Code Scanning category, so a difference means findings that never persist. See #2823.\n")
				failed = true
			}
		}
	}

	if failed {
		os.Exit(1)
	}
	fmt.Printf("Kubescape gate: %d required framework(s), identical sets across %d workflow(s).\n",
		len(requiredFrameworks), len(workflows))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "::error::"+format+"\n", args...)
	os.Exit(1)
}

// repoRoot walks up from the working directory to the first ancestor holding a
// .github/workflows directory.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".github", "workflows")); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .github/workflows directory found above the working directory; nothing was validated")
		}
		dir = parent
	}
}

// checkRequired asserts every required framework is a whole member of the set.
func checkRequired(set []string) error {
	present := make(map[string]bool, len(set))
	for _, f := range set {
		present[f] = true
	}
	missing := make([]string, 0, len(requiredFrameworks))
	for _, f := range requiredFrameworks {
		if !present[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"the Kubescape gate must evaluate %q, but --framework is %q. Dropping a framework REMOVES findings, so the compliance score RISES and CI stays green. See #2823",
		strings.Join(missing, ","), strings.Join(set, ","))
}

// frameworkSet returns one workflow's framework list, deduplicated and sorted so
// ordering and repetition cannot make two equal sets compare unequal.
func frameworkSet(workflow string) ([]string, error) {
	data, err := os.ReadFile(workflow) // #nosec G304 -- the workflow path is a CLI argument or the computed repo root, by design
	if err != nil {
		return nil, fmt.Errorf("%s could not be read; nothing was validated: %w", workflow, err)
	}

	scalars, err := runScalars(data)
	var parseErr yamlParseError
	if errors.As(err, &parseErr) {
		return nil, fmt.Errorf("%s could not be parsed as YAML; nothing was validated: %w", workflow, parseErr.err)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", workflow, err)
	}

	var invocations []string
	for _, scalar := range scalars {
		found, err := scanInvocations(scalar)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", workflow, err)
		}
		invocations = append(invocations, found...)
	}

	// An empty result from a filtered read is a claim about the FILTER, so zero
	// invocations is an error rather than a silent pass.
	if len(invocations) == 0 {
		return nil, fmt.Errorf(
			"no executable \"ksail workload scan --framework ...\" invocation found in %s. The gate moved, was renamed, or the framework list became a variable this guard cannot read. Point the guard at it rather than deleting the guard",
			workflow)
	}
	// MORE THAN ONE IS REJECTED, NOT MERGED: the union loses WHICH set produced
	// the SARIF that actually reaches the uploader.
	if len(invocations) > 1 {
		return nil, fmt.Errorf(
			"%s has %d scan invocations; the guard cannot tell which one produces the uploaded SARIF. Both workflows upload under one Code Scanning category, so the uploaded set is what must match. See #2823",
			workflow, len(invocations))
	}

	argument, err := frameworkArgument(invocations[0])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", workflow, err)
	}
	return frameworkTokens(argument, workflow)
}

// runScalars returns every `run:` scalar reachable in the parsed document.
//
// THIS IS STRUCTURAL, and that is the point: a mention of a command in a comment,
// a step `name:`, or a quoted `with:` value is not a `run:` scalar and cannot
// reach this list. The predecessor read raw file text, where all three matched.
// executingShells are the `shell:` values whose `run:` scalar this guard can read as a
// bash program AND which actually EXECUTE it. Anything else -- a custom `command {0}`
// template, or another language's interpreter -- means the scalar is not bash source, so
// finding a scan invocation in it says nothing about what runs.
//
// A CUSTOM TEMPLATE NEED NOT RUN THE SCRIPT AT ALL: `shell: cat {0}` merely PRINTS the
// generated file and exits 0, so a full-framework decoy declared that way never executes
// while the guard credited it as the gate. Measured. Refused rather than skipped: a real
// scan someone moved to an unusual shell should fail loudly, not vanish.
var executingShells = map[string]bool{"bash": true, "sh": true}

// effectiveShell resolves the `shell:` that applies to a step: the step's own, else the
// job's `defaults.run.shell`, else the workflow's. An empty result means none was
// declared, which on the runners this repository uses is bash.
func effectiveShell(root, job, step *yaml.Node) string {
	for _, n := range []*yaml.Node{
		mappingValue(step, "shell"),
		mappingValue(mappingValue(mappingValue(job, "defaults"), "run"), "shell"),
		mappingValue(mappingValue(mappingValue(root, "defaults"), "run"), "shell"),
	} {
		if n != nil && n.Kind == yaml.ScalarNode && strings.TrimSpace(n.Value) != "" {
			return strings.TrimSpace(n.Value)
		}
	}
	return ""
}

// yamlParseError marks a workflow that could not be parsed, as distinct from a
// refusal runScalars reaches after reading it (#3339).
type yamlParseError struct{ err error }

func (e yamlParseError) Error() string { return e.err.Error() }
func (e yamlParseError) Unwrap() error { return e.err }

func runScalars(data []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, yamlParseError{err}
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	jobs := mappingValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []string
	// Job VALUES sit at odd indices of a mapping's Content.
	for i := 1; i < len(jobs.Content); i += 2 {
		job := jobs.Content[i]
		// A JOB-level `if:` decides whether the runner starts the job at all, and a
		// STEP-level one whether it starts that step -- both are evaluated BEFORE any
		// shell exists, so no amount of shell parsing below can see them. A skipped step
		// carrying the full framework list would otherwise satisfy the gate while the
		// only scan that runs is a reduced one, which is the same fail-open as the `&&`
		// case and reached even earlier.
		//
		// Reachability is not decidable here (an `if:` may reference contexts this guard
		// cannot evaluate), so a conditional CANDIDATE is refused by name rather than
		// guessed at -- the direction this guard takes everywhere else. Only a step whose
		// `run:` actually carries a scan is affected: an ordinary conditional step in the
		// same workflow is untouched.
		// JOB level is NOT rejected merely for being conditional. The real `validate` job
		// carries a legitimate path-filter `if:` (it runs only when the manifests changed),
		// so refusing every conditional job would reject the very workflow this guard
		// validates -- measured, all three real-workflow tests failed on it.
		//
		// General reachability of a workflow expression is not decidable here, so what is
		// refused is bounded to what the text alone decides: a constant-false literal, and
		// a comparison of two literals that is constant-false. `${{ 1 == 2 }}` is refused;
		// `${{ 1 == '1' }}` is NOT, because Actions coerces across types, so that job
		// really runs and refusing it would be wrong. An expression naming a context, or a
		// compound expression, stays accepted: it cannot be decided here, and refusing it
		// would reject legitimate gates like the path filter above.
		jobConditional := constantFalse(mappingValue(job, "if"))
		steps := mappingValue(job, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, step := range steps.Content {
			v := mappingValue(step, "run")
			if v == nil || v.Kind != yaml.ScalarNode {
				continue
			}
			// STEP level IS rejected for being conditional at all: no real scan step carries
			// an `if:`, so this costs nothing and needs no reachability guess.
			if jobConditional || mappingValue(step, "if") != nil {
				if scanCandidate(v.Value) {
					return nil, fmt.Errorf(
						"a `run:` block invoking the scan is guarded by a workflow-level `if:`, so the runner decides whether it executes before any shell starts and this guard cannot see that decision: %q. A skipped step would let a full framework list stand in for a reduced scan that actually runs. Invoke the scan from an unconditional step in an unconditional job. See #2823",
						firstScanLine(v.Value))
				}
				continue
			}
			// The shell is resolved only for a step that actually carries a scan, so an
			// ordinary `shell: python` step elsewhere in the workflow is untouched.
			if sh := effectiveShell(root, job, step); sh != "" && !executingShells[sh] {
				if scanCandidate(v.Value) {
					return nil, fmt.Errorf(
						"a `run:` block invoking the scan declares `shell: %s`, so its text is not the bash program this guard reads and may not be executed at all — a custom `command {0}` template can simply print the script and exit 0: %q. Invoke the scan from a step using the default shell. See #2823",
						sh, firstScanLine(v.Value))
				}
				continue
			}
			out = append(out, v.Value)
		}
	}
	return out, nil
}

// firstScanLine returns the line naming the scan, for a diagnosable refusal.
func firstScanLine(scalar string) string {
	for _, line := range strings.Split(scalar, "\n") {
		if strings.Contains(line, "--framework") {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(scalar)
}

// mappingValue returns the value node for key in a mapping, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Kind == yaml.ScalarNode && n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// provablyFails reports whether a command is CERTAIN to exit non-zero, read from the
// text alone. Only `false` and `exit <literal>` qualify; a variable, a substitution
// or any other command does not, however likely it is to fail.
//
// THE LITERAL IS NOT THE STATUS. A shell exit code is taken modulo 256, so `exit 256`
// and `exit -256` both leave status 0 — a `|| exit 256` reads as re-raising the
// failure while actually swallowing it, which is the very bypass this whitelist
// exists to refuse. Compare the NORMALISED status rather than the written number.
func provablyFails(text string) bool {
	fields := strings.Fields(text)
	switch {
	case len(fields) == 1 && fields[0] == "false":
		return true
	case len(fields) == 2 && fields[0] == "exit":
		code, err := strconv.Atoi(fields[1])
		if err != nil {
			return false
		}
		// Go's % keeps the sign of the dividend, so -256%256 is 0 but -1%256 is -1;
		// the second +256 %256 folds a negative remainder into 0..255 as the shell does.
		return ((code%256)+256)%256 != 0
	}
	return false
}

// Shell words that introduce a COMPOUND command — one whose body's execution is not
// decidable from the text. Deliberately limited to reachability constructs: `[[`, `!` and
// `time` change no command's reachability and would refuse ordinary tests for nothing.
//
// `in` is absent because it is never a COMMAND, only a word inside `for`/`case` — and
// those two are here, so the construct is caught at its keyword.
var shellCompoundWords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "select": true,
	"function": true, "coproc": true,
	"{": true, "}": true, "(": true, ")": true,
}

// compoundToken returns the token introducing a compound command when this segment does,
// or "" when it is a plain simple command.
//
// 🔴 ONLY THE FIRST TOKEN, because that is the only place the shell recognises a reserved
// word. Scanning every token instead reads an ARGUMENT as a keyword: `echo done` is a
// simple command, and `ksail workload scan … && echo done` was refused for the `done`
// belonging to `echo`. `shellSplit` has already ended a segment at each operator, so the
// first token of every segment is genuinely in command position.
//
// Whole-token equality, never a substring: the shipped invocation ends in
// `-o "${RUNNER_TEMP}/kubescape.sarif"`, which contains `{` and `}` inside a quoted word.
// `shellSplit` preserves the quote characters, so that word is one token and matches
// nothing here — while a brace GROUP, whose `{` is a token on its own, does.
func compoundToken(segment string) string {
	fields := strings.Fields(segment)
	if len(fields) == 0 {
		return ""
	}
	head := fields[0]
	if shellCompoundWords[head] {
		return head
	}
	// 🔴 A SHAPE TEST, NOT A SUFFIX TEST. A simple command's NAME never contains an
	// unquoted `(`, `)`, `{` or `}`, so any of them here means the segment opens a
	// function definition, a subshell or a group — whatever the spacing.
	//
	// A `strings.HasSuffix(head, "()")` test missed `unused(){`, where bash accepts the
	// brace with no space and the whole thing arrives as ONE field. That form happened to
	// be refused anyway, by the closing `}` landing as its own segment — accidental
	// coverage that a one-line body would not have had, so it is closed at the opener.
	//
	// Quoted spans are skipped, because a first token legitimately carries these
	// characters inside quotes: `"${TOOL}" run` is an ordinary command invocation.
	if unquotedGroupingChar(head) != "" {
		return head
	}
	return ""
}

// unquotedGroupingChar returns the first `(`, `)`, `{` or `}` in tok that is not inside a
// single- or double-quoted span, or "" when there is none.
func unquotedGroupingChar(tok string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle || inDouble:
		case c == '(' || c == ')' || c == '{' || c == '}':
			return string(c)
		}
	}
	return ""
}

// scanInvocations returns the commands in one `run:` scalar that actually INVOKE the
// scan — one entry per invocation, so two scans are two entries and the caller rejects the
// ambiguity rather than judging the upload on the first.
//
// Shell CONTEXT is the parse tree's: heredoc bodies, quoted strings spanning lines,
// comments and continuations are where bash puts them, so none of them is re-derived here.
//
// Command SHAPE: a command's first three words must BE `ksail workload scan`. Anything else
// — `echo`, `env ksail ...`, a quoted argument that merely contains the words — is not a
// bare invocation, and is refused when it could execute one the guard never reads.
//
// Shell CONTROL FLOW is closed by REJECTING rather than by evaluating: a scan behind
// `&&`/`||`, inside a compound command or substitution, or whose status is discarded, is a
// form this cannot read, and counting it as executed is what makes it dangerous.
func scanInvocations(scalar string) ([]string, error) {
	script, err := analyseScript(scalar)
	if err != nil {
		return nil, fmt.Errorf(
			"a `run:` block is not a bash program this guard can read, so what it executes is not decidable from the text — %v. Past an unreadable construct such as a heredoc opener with no delimiter, every following line has undecidable status, and a body read as code would let a non-executing decoy supply the framework list. See #2823",
			err)
	}
	var out []string
	for _, site := range script.sites {
		fields := site.fields
		text := rawText(fields)
		if !scanCommand(fields) {
			// A COMMAND STRING HANDED TO AN EXECUTING SHELL IS ONE QUOTED ARGUMENT, so the
			// word rules below never see the invocation inside it.
			if reason := undecidableShellString(fields); reason != "" {
				return nil, fmt.Errorf(
					"a command string handed to an executing shell names a scan word, so whether it invokes `ksail workload scan --framework` is not decidable from the text — %s: %q. The interpreter runs that string as code, so a reduced scan inside it executes while the guard reads it as an argument. Invoke the scan as a plain command, never through `eval` or `<shell> -c`. See #3338",
					reason, text)
			}
			// EVERY COMMAND WORD EXPANDED AT ONCE on a `--framework` command.
			if framed(fields) && allExpandedBeforeFramework(fields) {
				return nil, fmt.Errorf(
					"every word before `--framework` is a shell expansion, so the command this line runs is not decidable from the text: %q. Variables assigned earlier in the block can spell `ksail workload scan` here, executing a reduced scan the guard never read. Invoke the scan as the bare words `ksail workload scan`, or quote the text if it is not an invocation. See #3338",
					text)
			}
			// ANY PLAIN SCAN WORD BESIDE ANY EXPANSION IS REFUSED.
			if reason := undecidableScanCandidate(fields); reason != "" {
				return nil, fmt.Errorf(
					"a line names `ksail`, `workload` or `scan` plainly beside a shell expansion, so whether it invokes `ksail workload scan --framework` is not decidable from the text — %s: %q. A constructed command or option word can execute a scan while reading as something else, so the executed framework set need not be the validated one. Spell the whole invocation plainly, or quote the text if it is not an invocation. See #3338",
					reason, text)
			}
			if framed(fields) {
				if reason := undecidableCommandWord(fields); reason != "" {
					return nil, fmt.Errorf(
						"the command word of a `--framework` line is not decidable from the text — %s: %q. A shell expansion or an unrecognised spelling in command position can execute `ksail workload scan` while reading as something else, so the executed framework set need not be the validated one. Invoke the scan as the bare word `ksail` with no expansion before `--framework`, or quote the text if it is not an invocation. See #3338",
						reason, text)
				}
			}
			if j, ok := workloadScanArgs(fields); ok {
				if reason := undecidableOptionWord(fields[j:]); reason != "" {
					return nil, fmt.Errorf(
						"an argument of a `workload scan` candidate is not decidable from the text — %s: %q. A shell expansion or a quoted spelling in an option word can execute `--framework` while reading as something else, so the executed framework set need not be the validated one. Spell every option word plainly, or quote the text if it is not an invocation. See #3338",
						reason, text)
				}
			}
			// A PREFIXED INVOCATION STILL RUNS (`env`, `sudo`, an assignment, `echo`): refused,
			// keyed on the three scan words rather than on a blacklist of prefixes.
			if prefixedScan(fields) && framed(fields) {
				return nil, fmt.Errorf(
					"a `ksail workload scan --framework` invocation is preceded by another command or an environment assignment, so it executes without being validated: %q. Paired with a countable invocation this lets the validated framework set differ from the one that actually runs. Invoke the scan with no prefix, or quote the text if it is not an invocation. See #3338",
					text)
			}
			continue
		}
		if reason := undecidableOptionWord(fields[3:]); reason != "" {
			return nil, fmt.Errorf(
				"an argument of a `ksail workload scan` invocation is not decidable from the text — %s: %q. A shell expansion or a quoted spelling in an option word can execute `--framework` while reading as something else, so the executed framework set need not be the validated one. Spell every option word plainly, with any expansion confined to a double-quoted option value. See #3338",
				reason, text)
		}
		if !framed(fields) {
			continue
		}
		if site.nested != "" {
			return nil, fmt.Errorf(
				"a scan invocation sits inside %s, so whether it executes is not decidable from the text: %q. A body the shell may skip, or a substitution whose output is discarded, would let an unreachable full framework list stand in for a reduced scan that actually runs. Invoke the scan as a plain command, in a block of plain commands. See #2823",
				site.nested, text)
		}
		if site.conditional {
			return nil, fmt.Errorf(
				"a scan invocation is guarded by `&&` or `||`, so whether it runs depends on another command's exit status: %q. A conditionally executed scan is not evidence of what the gate runs — an always-false guard would let a full framework list stand in for a reduced scan that actually executes. Invoke the scan unconditionally. See #2823",
				text)
		}
		// A RUNNING SCAN IS NOT A GATE IF ITS FAILURE IS DISCARDED.
		if site.masked {
			return nil, fmt.Errorf(
				"a scan invocation's exit status is discarded by the `&`, `|`, `||` or `!` around it, so its failure does not fail the step: %q. A scan whose failure is ignored is not a gate — it would let a full framework list stand in for a reduced scan that actually decides the outcome. Invoke the scan as a plain command whose status the step sees. See #2823",
				text)
		}
		out = append(out, text)
	}
	if len(out) > 0 && script.multiline != "" {
		return nil, fmt.Errorf(
			"a scan invocation shares its `run:` block with a %s substitution spanning lines, so whether the scan executes is not decidable from the text: a `false &&` before the newline suppresses it while the line still reads as an unconditional invocation, letting a full framework list stand in for a reduced scan that actually runs. Close the substitution on its own line and invoke the scan as a plain command outside it. See #2823",
			script.multiline)
	}
	if len(out) > 0 && script.compound != "" {
		return nil, fmt.Errorf(
			"a scan invocation shares its `run:` block with the compound command %q, so whether it executes is not decidable from the text. An `if`, loop, `case`, function or group body is skipped without any operator on the scan's own line, which would let an unreachable full framework list stand in for a reduced scan that actually runs. Invoke the scan as a plain command, in a block of plain commands. See #2823",
			script.compound)
	}
	return out, nil
}

// undecidableShellString names an `eval`, or a shell interpreter given a `-c`-style
// option, whose following arguments name a scan word — text the interpreter will run
// as code, which no per-word rule can read — or returns "" when there is none.
func undecidableShellString(fields []shWord) string {
	for i, f := range fields {
		word := f.value
		base := word[strings.LastIndex(word, "/")+1:]
		executes := word == "eval"
		if !executes {
			switch base {
			case "bash", "sh", "dash", "zsh", "ksh":
				skipOperand := false
				for j := i + 1; j < len(fields); j++ {
					if skipOperand {
						skipOperand = false
						continue
					}
					opt := fields[j].value
					if !strings.HasPrefix(opt, "-") || opt == "-" || opt == "--" {
						break
					}
					if !strings.HasPrefix(opt, "--") && strings.Contains(opt, "c") {
						executes = true
						break
					}
					if opt == "-o" || (base == "bash" && (opt == "-O" || opt == "--init-file" || opt == "--rcfile")) {
						skipOperand = true
					}
				}
			}
		}
		if !executes {
			continue
		}
		for _, arg := range fields[i+1:] {
			if literal := arg.value; literalShellWords.MatchString(literal) {
				// A filename or identifier CONTAINING "scan" is not the word scan.
				named := false
				for _, token := range strings.Fields(literal) {
					switch filepath.Base(token) {
					case "ksail", "workload", "scan":
						named = true
					}
				}
				if !named {
					continue
				}
			}
			if strings.Contains(arg.raw, "ksail") || strings.Contains(arg.raw, "workload") || strings.Contains(arg.raw, "scan") {
				return fmt.Sprintf("%q executes its argument as shell code and that argument names a scan word", word)
			}
		}
	}
	return ""
}

// allExpandedBeforeFramework reports whether every word in front of the first
// `--framework` word carries a shell expansion.
func allExpandedBeforeFramework(fields []shWord) bool {
	seen := 0
	for _, f := range fields {
		if strings.HasPrefix(f.value, "--framework") {
			break
		}
		if !f.expands {
			return false
		}
		seen++
	}
	return seen > 0
}

// prefixedScan reports whether `ksail workload scan` appears as three consecutive words
// somewhere OTHER than command position.
func prefixedScan(fields []shWord) bool {
	for i := 1; i+2 < len(fields); i++ {
		if fields[i].value == "ksail" && fields[i+1].value == "workload" && fields[i+2].value == "scan" {
			return true
		}
	}
	return false
}

// undecidableCommandWord names why a `--framework` command's command word cannot be read
// from the text, or returns "" when it can. A WHITELIST: every word before `--framework`
// is expansion-free once a scan word stands there, and the word in front of `workload
// scan` resolves to exactly `ksail`.
func undecidableCommandWord(fields []shWord) string {
	start := 0
	if j, ok := workloadScanArgs(fields); ok {
		start = j
	}
	fw := -1
	for i := start; i < len(fields); i++ {
		if framed(fields[i : i+1]) {
			fw = i
			break
		}
	}
	if fw < 0 {
		return ""
	}
	scanWord := false
	for _, f := range fields[:fw] {
		switch f.value {
		case "ksail", "workload", "scan":
			scanWord = true
		}
	}
	if scanWord {
		for _, f := range fields[:fw] {
			if f.expands {
				return fmt.Sprintf("%q carries a shell expansion", f.raw)
			}
		}
	}
	for j := 1; j+1 < fw; j++ {
		if fields[j].value != "workload" || fields[j+1].value != "scan" {
			continue
		}
		if fields[j-1].value != "ksail" {
			return fmt.Sprintf("%q in front of `workload scan` is not the bare word ksail", fields[j-1].raw)
		}
	}
	return ""
}

// undecidableScanCandidate reports why a command that is not a plainly spelled primary
// invocation still cannot be read as ordinary shell: it names a scan word (`ksail`,
// `workload` or `scan`) as a whole resolved word AND carries an expansion in some word.
// A quoted multi-word string is one word whose value holds spaces, so prose never matches.
func undecidableScanCandidate(fields []shWord) string {
	scanWord := ""
	for _, f := range fields {
		switch f.value {
		case "ksail", "workload", "scan":
			if scanWord == "" {
				scanWord = f.raw
			}
		}
	}
	if scanWord == "" {
		return ""
	}
	for _, f := range fields {
		if f.expands {
			return fmt.Sprintf("%q is a plain scan word and %q carries a shell expansion", scanWord, f.raw)
		}
	}
	return ""
}

// undecidableOptionWord names why the ARGUMENTS of a scan invocation cannot be read from
// the text, or returns "" when they can. Every option word is a plain word; an expansion is
// accepted in exactly one shape — the whole double-quoted value of `-o`/`--output`.
func undecidableOptionWord(args []shWord) string {
	outputOption := func(tok string) bool {
		return tok == "-o" || tok == "--output"
	}
	for i, tok := range args {
		switch {
		case !tok.expands && !tok.spelled:
			continue
		case strings.HasPrefix(tok.value, "-") || strings.HasPrefix(tok.raw, "-"):
			return fmt.Sprintf("option word %q is not a plain token", tok.raw)
		case tok.expands && tok.doubleQuoted && i > 0 && outputOption(args[i-1].raw):
			continue
		case tok.expands:
			return fmt.Sprintf("argument %q carries a shell expansion or pattern outside a double-quoted -o/--output value", tok.raw)
		default:
			// A quoted non-option argument (`'nsa,mitre'`) cannot construct an option word;
			// frameworkTokens still refuses it as a value.
			continue
		}
	}
	return ""
}

// workloadScanArgs returns the index of the first word after a `workload scan` pair and
// whether one is present.
func workloadScanArgs(fields []shWord) (int, bool) {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i].value == "workload" && fields[i+1].value == "scan" {
			return i + 2, true
		}
	}
	return 0, false
}

// frameworkArgument returns the raw `--framework` value of one invocation.
//
// THE VALUE IS READ TO THE NEXT SPACE, not through a class listing the characters
// a framework name may contain. Such a class does not FAIL on an unexpected
// character, it TRUNCATES at it: `nsa,mitre,cis-v1.23-t1.0.1` and
// `nsa,mitre,cis-v1.24-t1.0.0` both normalised to `cis,mitre,nsa`, so two
// workflows scanning genuinely different sets compared EQUAL.
func frameworkArgument(invocation string) (string, error) {
	fields := strings.Fields(invocation)
	for i, f := range fields {
		if f == "--framework" && i+1 < len(fields) {
			return fields[i+1], nil
		}
		if v, ok := strings.CutPrefix(f, "--framework="); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("could not read the --framework value")
}

// frameworkTokens splits the argument on commas and fails closed on any token
// that is not a plain framework name. A `--framework "$FRAMEWORKS"` variable form
// lands here as `"$FRAMEWORKS"`, fails the pattern, and trips the fail-closed
// path — which is the point.
func frameworkTokens(argument, workflow string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	for _, token := range strings.Split(argument, ",") {
		if token == "" {
			continue
		}
		if !frameworkToken.MatchString(token) {
			return nil, fmt.Errorf(
				"framework token %q is not a plain framework name. The guard reads the literal list; a variable or expression cannot be verified. See #2823",
				token)
		}
		if !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: the --framework list read as empty", workflow)
	}
	sort.Strings(out)
	return out, nil
}

// constantFalse reports whether a workflow `if:` can never be true.
//
// Deliberately narrow. Reachability of a real expression is not decidable here, and
// refusing every conditional job rejects the real `validate` job's legitimate path
// filter -- measured. So a verdict is taken only where LITERAL operands settle it;
// anything naming a context stays undecidable and therefore ACCEPTED.
func constantFalse(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.ScalarNode {
		return false
	}
	v := strings.TrimSpace(n.Value)
	v = strings.TrimPrefix(v, "${{")
	v = strings.TrimSuffix(v, "}}")
	return evalCondition(v) == triFalse
}

// A three-valued result. triUnknown is the ACCEPTING verdict: it means this guard
// cannot decide the condition, so the job is read as if it runs.
const (
	triFalse = iota
	triTrue
	triUnknown
)

// evalCondition evaluates the boolean skeleton of an `if:` over literal operands,
// in Actions' precedence order: `||` binds loosest, then `&&`, then `!`.
//
// A compound condition is decidable far more often than a bare literal is, and a
// job Actions skips must never supply the framework set -- `${{ false && true }}`
// is skipped exactly as `if: false` is, so a never-running decoy could otherwise
// stand in for a reduced scan that actually runs.
func evalCondition(expr string) int {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return triUnknown
	}
	if parts := splitTopLevel(expr, "||"); len(parts) > 1 {
		acc := evalCondition(parts[0])
		for _, part := range parts[1:] {
			acc = orTri(acc, evalCondition(part))
		}
		return acc
	}
	if parts := splitTopLevel(expr, "&&"); len(parts) > 1 {
		acc := evalCondition(parts[0])
		for _, part := range parts[1:] {
			acc = andTri(acc, evalCondition(part))
		}
		return acc
	}
	if strings.HasPrefix(expr, "!") {
		return notTri(evalCondition(expr[1:]))
	}
	if inner, ok := unwrapParens(expr); ok {
		return evalCondition(inner)
	}
	switch strings.ToLower(expr) {
	case "false", "'false'", "\"false\"", "0":
		return triFalse
	case "true", "'true'", "\"true\"":
		return triTrue
	}
	return comparisonTri(expr)
}

// andTri follows Actions' semantics: `a && b` yields a when a is falsy, else b. So an
// undecidable left with a FALSE right is falsy either way, and is decided here.
func andTri(l, r int) int {
	switch l {
	case triFalse:
		return triFalse
	case triTrue:
		return r
	}
	if r == triFalse {
		return triFalse
	}
	return triUnknown
}

// orTri mirrors it: `a || b` yields a when a is truthy, else b. An undecidable left
// with a TRUE right is truthy either way.
func orTri(l, r int) int {
	switch l {
	case triTrue:
		return triTrue
	case triFalse:
		return r
	}
	if r == triTrue {
		return triTrue
	}
	return triUnknown
}

func notTri(v int) int {
	switch v {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triUnknown
}

// unwrapParens strips ONE fully-enclosing parenthesis pair. It reports false when the
// leading `(` is closed before the end -- `(a) && (b)` is not a parenthesised whole,
// and treating it as one would evaluate the wrong sub-expression.
func unwrapParens(expr string) (string, bool) {
	if !strings.HasPrefix(expr, "(") || !strings.HasSuffix(expr, ")") {
		return "", false
	}
	depth, quote := 0, byte(0)
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(expr)-1 {
				return "", false
			}
		}
	}
	if depth != 0 {
		return "", false
	}
	return strings.TrimSpace(expr[1 : len(expr)-1]), true
}

// splitTopLevel splits on an operator only where it is STRUCTURE: outside every string
// literal and at parenthesis depth zero. An operator inside `'a&&b'` is content, and
// tearing the literal apart there would silently change what is being compared.
func splitTopLevel(expr, op string) []string {
	var parts []string
	depth, quote, last := 0, byte(0), 0
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			continue
		case '(':
			depth++
			continue
		case ')':
			depth--
			continue
		}
		if depth == 0 && strings.HasPrefix(expr[i:], op) {
			parts = append(parts, strings.TrimSpace(expr[last:i]))
			i += len(op) - 1
			last = i + 1
		}
	}
	// An unterminated quote or unbalanced parenthesis means this is not a shape we
	// parsed correctly; report no split so the caller falls through to undecidable.
	if quote != 0 || depth != 0 {
		return []string{expr}
	}
	parts = append(parts, strings.TrimSpace(expr[last:]))
	return parts
}

// comparisonTri decides a comparison of two LITERALS of the same kind, and reports
// triUnknown for everything else.
func comparisonTri(expr string) int {
	var op string
	switch {
	case strings.Contains(expr, "=="):
		op = "=="
	case strings.Contains(expr, "!="):
		op = "!="
	default:
		return triUnknown
	}
	parts := strings.SplitN(expr, op, 2)
	if len(parts) != 2 {
		return triUnknown
	}
	leftKind, leftVal, leftOK := literalOperand(parts[0])
	rightKind, rightVal, rightOK := literalOperand(parts[1])
	if !leftOK || !rightOK || leftKind != rightKind {
		return triUnknown
	}
	// ACTIONS IGNORES CASE WHEN COMPARING STRINGS, so `'A' == 'a'` is TRUE there.
	// Comparing case-sensitively decided the opposite, and the direction is a
	// fail-open: a job guarded by `${{ 'A' != 'a' }}` is SKIPPED by Actions, but a
	// case-sensitive `!=` called it reachable, so a decoy scan in that never-running
	// job was allowed to satisfy the framework gate. The other operand kinds are
	// already normalised above (bool and null are lowercased, numbers canonicalised),
	// so equality for them stays exact.
	equal := leftVal == rightVal
	if leftKind == "string" {
		equal = strings.EqualFold(leftVal, rightVal)
	}
	if op == "==" {
		if equal {
			return triTrue
		}
		return triFalse
	}
	if !equal {
		return triTrue
	}
	return triFalse
}

// literalOperand classifies one side of a comparison as a literal, returning its
// kind and a normalised value. A non-literal -- any context reference, function
// call, or compound expression -- reports false so the comparison stays undecidable.
func literalOperand(s string) (kind string, value string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			inner := s[1 : len(s)-1]
			// A quote INSIDE means this is not one simple literal (an escape, or a
			// larger expression that merely starts and ends with a quote).
			if strings.ContainsAny(inner, "'\"") {
				return "", "", false
			}
			return "string", inner, true
		}
	}
	switch strings.ToLower(s) {
	case "true", "false":
		return "bool", strings.ToLower(s), true
	case "null":
		return "null", "null", true
	}
	// Numbers are normalised so `1` and `1.0` compare equal, as Actions treats them.
	if numericLiteral.MatchString(s) {
		return "number", normaliseNumber(s), true
	}
	return "", "", false
}

var numericLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// normaliseNumber trims a trailing fractional zero run so `1.0` and `1` agree.
func normaliseNumber(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
