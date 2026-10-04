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
// Workflow structure selects run scalars. One Bash syntax tree then discovers
// command positions, nested execution and failure handling for both conditional
// admission and framework extraction. Interpreter code inputs have explicit
// bounded classifiers; the validator never executes or expands shell source.
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

	scalars, err := workflowScans(data)
	var parseErr yamlParseError
	if errors.As(err, &parseErr) {
		return nil, fmt.Errorf("%s could not be parsed as YAML; nothing was validated: %w", workflow, parseErr.err)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", workflow, err)
	}

	var invocations []string
	for _, scalar := range scalars {
		if scalar.err != nil {
			return nil, fmt.Errorf("%s: %w", workflow, scalar.err)
		}
		invocations = append(invocations, scalar.scans...)
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

	return frameworkTokens(invocations[0], workflow)
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

func workflowScans(data []byte) ([]shellAnalysis, error) {
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
	var out []shellAnalysis
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
			parsed := analyzeShell(v.Value)
			sh := effectiveShell(root, job, step)
			supportedShell := sh == "" || executingShells[sh]
			// STEP level IS rejected for being conditional at all: no real scan step carries
			// an `if:`, so this costs nothing and needs no reachability guess.
			if jobConditional || mappingValue(step, "if") != nil {
				if parsed.candidate || (supportedShell && parsed.err != nil) {
					return nil, fmt.Errorf(
						"a `run:` block invoking the scan is guarded by a workflow-level `if:`, so the runner decides whether it executes before any shell starts and this guard cannot see that decision: %q. A skipped step would let a full framework list stand in for a reduced scan that actually runs. Invoke the scan from an unconditional step in an unconditional job. See #2823",
						firstScanLine(v.Value))
				}
				continue
			}
			// The shell is resolved only for a step that actually carries a scan, so an
			// ordinary `shell: python` step elsewhere in the workflow is untouched.
			if !supportedShell {
				if parsed.candidate {
					return nil, fmt.Errorf(
						"a `run:` block invoking the scan declares `shell: %s`, so its text is not the bash program this guard reads and may not be executed at all — a custom `command {0}` template can simply print the script and exit 0: %q. Invoke the scan from a step using the default shell. See #2823",
						sh, firstScanLine(v.Value))
				}
				continue
			}
			out = append(out, parsed)
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

// prefixedScan reports whether `ksail workload scan` appears as three consecutive
// tokens somewhere OTHER than command position — the shape a leading environment
// assignment or wrapper command produces (`env ksail ...`, `env FOO=1 ksail ...`,
// `sudo ksail ...`).
//
// Keyed on the three scan tokens rather than on a list of wrapper words. A blacklist
// of prefixes would have to enumerate every spelling, and the one it missed would be
// the one that mattered; keying on the scan itself has no such gap. A segment that
// does NOT carry those tokens is ordinary shell and is left alone.
// bareToken resolves a shell token to the literal WORD the shell would execute,
// so every spelling of one command name collapses to a single value before
// matching. `'ksail'`, `k"s"ail`, `k's'ail` and `k\sail` are all the word `ksail`.
//
// Quotes and backslashes are the only constructs resolved, and that is the whole
// DECIDABLE set: `$(…)`, backticks and `${…}` need the shell's own evaluation to
// know what word they produce, and scanInvocations refuses those separately
// rather than guessing at them here.
//
// UNBALANCED quoting returns the token UNCHANGED, and that restriction is what
// keeps the check honest rather than being a shortfall: `shellSplit` preserves
// quote characters, so a multi-word quoted string arrives as tokens whose quotes
// never close (`'ksail` … `nsa'`). Leaving those alone is what stops
// `echo 'ksail workload scan --framework nsa'` — echoed text rather than an
// execution — being refused as a prefixed scan.
//
// Quote CONTEXT is honoured, which is what preserves the one-level rule: inside
// double quotes a single quote is literal, so `"'ksail'"` resolves to the word
// `'ksail'`, which names a different command than ksail.
func bareToken(tok string) string {
	word, _ := resolveToken(tok)
	return word
}

// resolveToken is bareToken with its one refusal made visible: fragment is true when
// the token was returned UNCHANGED because its quotes never closed, which is what
// marks it as a piece of a longer quoted string rather than a command name.
func resolveToken(tok string) (word string, fragment bool) {
	const (
		plain = iota
		single
		double
	)

	var b strings.Builder
	b.Grow(len(tok))

	state := plain
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch state {
		case plain:
			switch c {
			case '\'':
				state = single
			case '"':
				state = double
			case '\\':
				// Escapes the next byte literally. A TRAILING backslash is a line
				// continuation, which contributes no character at all.
				if i+1 < len(tok) {
					i++
					b.WriteByte(tok[i])
				}
			default:
				b.WriteByte(c)
			}
		case single:
			// Nothing is special inside single quotes, a backslash least of all.
			if c == '\'' {
				state = plain
				continue
			}
			b.WriteByte(c)
		case double:
			switch c {
			case '"':
				state = plain
			case '\\':
				// Inside double quotes a backslash escapes only these four; before
				// anything else it stays a literal backslash.
				if i+1 < len(tok) && (tok[i+1] == '"' || tok[i+1] == '\\' ||
					tok[i+1] == '$' || tok[i+1] == '`') {
					i++
					b.WriteByte(tok[i])
					continue
				}
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		}
	}

	// Quotes that never closed mean this token is a fragment of a longer quoted
	// string, not a command name — see the doc comment above.
	if state != plain {
		return tok, true
	}
	return b.String(), false
}

// allExpandedBeforeFramework reports whether every token in front of the first
// `--framework` word carries a shell expansion — a command spelled entirely from
// variables, which resolves to a plain scan word only when the shell runs.
func allExpandedBeforeFramework(fields []string) bool {
	seen := 0
	for _, f := range fields {
		if strings.HasPrefix(bareToken(f), "--framework") {
			break
		}
		if !carriesExpansion(f) {
			return false
		}
		seen++
	}
	return seen > 0
}

// undecidableOptionWord names why the ARGUMENTS of a scan invocation cannot be read from
// the text, or returns "" when they can.
//
// The command-word whitelist above stops at `--framework`, and that was the hole: the
// option word itself can be built by the shell. `--frame${SUFFIX} nsa`, `--frame$'work'`
// and `--frame"work"` all execute as `--framework` while the raw text carries no such
// token, so the primary-scan path skipped the line as an unframed scan and, paired with a
// counted invocation, credited the wrong set. A bare `$EXTRA` after the flag is the same
// hole from the other side: it can expand to a second `--framework` that overrides the
// one the guard read.
//
// Again a WHITELIST of what a decidable argument list LOOKS like, not a list of
// expansion syntaxes. Every option word is a plain token: no quotes, no backslashes, no
// `$`, no backtick. A token carrying an expansion is accepted in exactly one shape — the
// VALUE of the plain option word before it, wrapped whole in double quotes so it can
// never split into extra words — because the real workflow writes its output path as
// `-o "${RUNNER_TEMP}/kubescape.sarif"`, and refusing that would fail the known-good
// configuration on the rule's first run. Everything else is refused.
// undecidableScanCandidate reports why a line that is not a plainly spelled primary
// invocation still cannot be read as ordinary shell: it names at least one scan word
// (`ksail`, `workload` or `scan`) as a plain, fully resolved token AND carries a shell
// expansion or pathname pattern in some token. Keyed on the plain word rather than on a
// literal `--framework` or a resolvable `workload scan` pair, because each of those
// anchors can be constructed away (`work${LOAD}`, `--frame${SUFFIX}`) while the line
// still executes a scan. A quoted fragment (`"ksail`) is not a plain word, so quoted
// prose stays readable; an empty reason means the line is decidable.
//
// THE PLAIN WORD MUST HAVE ROOM TO TAKE PART IN AN INVOCATION (#3585). Keying on the
// word alone refused `echo scan "$STATUS"`, an ordinary diagnostic line in which `scan`
// cannot belong to any `ksail workload scan` invocation: see roomForInvocation. Every
// plain `ksail`, and every `workload` or `scan` that does have room, still triggers the
// refusal below.
func undecidableScanCandidate(fields []string) string {
	// TOKENS INSIDE A MULTI-WORD QUOTED STRING ARE NOT PLAIN WORDS. resolveToken flags only
	// the token that opens or closes the string; the words between them resolve as plain
	// (`echo "ksail workload scan finished: $STATUS"` puts `workload` and `scan` there), so
	// the quote state is tracked across the line and everything inside it is skipped —
	// the quoting opt-out the messages name must keep working when the prose carries a
	// variable. A balanced token (`"$X"`, `--frame"work"`) is outside any such string.
	inQuote := false
	plain := make([]string, len(fields))
	// capacity[i] bounds how many argv words fields[i] can become. Quoted prose that a
	// line break left open counts as unlimited: its other end is not on this line.
	capacity := make([]int, len(fields))
	for i, f := range fields {
		word, fragment := resolveToken(f)
		if fragment {
			inQuote = !inQuote
			capacity[i] = unlimitedWords
			continue
		}
		if inQuote {
			capacity[i] = unlimitedWords
			continue
		}
		plain[i] = word
		capacity[i] = wordCapacity(f)
	}
	// before[i] is how many words the fields in front of position i can supply.
	before := make([]int, len(fields)+1)
	for i := range fields {
		before[i+1] = before[i] + capacity[i]
	}
	scanWord := ""
	for i, f := range fields {
		if roomForInvocation(plain[i], before[i], before[len(fields)]-before[i+1]) {
			scanWord = f
			break
		}
	}
	if scanWord == "" {
		return ""
	}
	inQuote = false
	for _, f := range fields {
		if _, fragment := resolveToken(f); fragment {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		if carriesExpansion(f) {
			return fmt.Sprintf("%q is a plain scan word and %q carries a shell expansion", scanWord, f)
		}
	}
	return ""
}

// carriesExpansion reports whether a single token can expand at run time: a `$` or a
// backtick outside single quotes, or an unquoted `*`, `?` or `[` (pathname expansion).
// Quote state matters, not merely the characters — `'$HOME'` is the four literal
// characters and `'*.yaml'` is a literal glob, so a line like `echo 'scan' '$HOME'`
// carries no expansion at all; a bare `ContainsAny` refused it beside the quoted scan
// word. Inside double quotes `$` and backticks still expand while `*?[` do not, and a
// backslash escapes the character after it in either the plain or the double-quoted
// state, exactly as resolveToken reads them. A token whose quotes never close is a
// fragment of a longer string; its expansion state is decided by the tokens around it,
// so it reports false here. An unquoted `{` is brace expansion — `--{framework=nsa,output=x}`
// becomes two options only when the shell runs — and counts as an expansion too;
// inside double quotes braces are literal.
func carriesExpansion(tok string) bool {
	quoted, unquoted := expansionKinds(tok)
	return quoted || unquoted
}

// expansionKinds splits carriesExpansion's answer by where the expansion sits: quoted
// is a `$` or backtick inside double quotes, unquoted is any expansion or pattern
// outside quotes. The distinction decides how many words the token can become — an
// unquoted expansion is word-split and globbed, a double-quoted one is not.
func expansionKinds(tok string) (quoted, unquoted bool) {
	const (
		plain = iota
		single
		double
	)
	state := plain
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch state {
		case plain:
			switch c {
			case '\'':
				state = single
			case '"':
				state = double
			case '\\':
				i++
			case '$', '`', '*', '?', '[', '{':
				unquoted = true
			}
		case single:
			if c == '\'' {
				state = plain
			}
		case double:
			switch c {
			case '"':
				state = plain
			case '\\':
				i++
			case '$', '`':
				quoted = true
			}
		}
	}
	return quoted, unquoted
}

// unlimitedWords is the capacity of a token that can expand to any number of words.
// Large enough that a sum of them can never fall below a role's need, small enough
// that summing a line's worth cannot overflow.
const unlimitedWords = 1 << 20

// wordCapacity bounds how many argv words one shell word can become. A word with no
// expansion is exactly one. An UNQUOTED expansion is split on whitespace, globbed and
// brace-expanded, so it is unlimited — and that includes the ANSI-C `$'…'` form, which
// is one word in fact but is not worth a special case on a check that must not
// under-count. A DOUBLE-QUOTED expansion is one word, with one exception the quotes do
// not prevent: `"$@"` and `"${a[@]}"` expand to one word per element, so any `@` in an
// expanding token is unlimited too.
func wordCapacity(tok string) int {
	quoted, unquoted := expansionKinds(tok)
	switch {
	case unquoted:
		return unlimitedWords
	case quoted && strings.Contains(tok, "@"):
		return unlimitedWords
	}
	return 1
}

// roomForInvocation reports whether a plain scan word, with `front` words available
// before it and `back` words after it, could take part in a `ksail workload scan`
// invocation. `workload` or `scan` may be a renamed binary, which needs `workload scan`
// after it — two words. `workload` may also be the subcommand, needing the binary in
// front and `scan` behind; `scan` may also be the subcommand, needing the binary and
// `workload` in front.
//
// `ksail` ITSELF IS ALWAYS EVIDENCE, whatever surrounds it. It names the binary, and a
// wrapper that appends arguments it reads at run time (`xargs ksail`, see #4164) gives
// it room the line does not show. Narrowing it would buy nothing #3585 asked for. A
// renamed binary fed its arguments that way is the residual every name shares — it is
// as open for `scan2` as for `scan` — so a scan word needs no stricter count there.
//
// THE COUNT IS POSITIONAL, NOT LEXICAL. It never asks whether a neighbouring word
// spells `workload` or names a wrapper, so a subcommand alias, a wrapper such as `env`
// or `sudo`, or an option between the words cannot make it under-count: every word
// counts as able to fill a role. It answers "no" only where no reading of the line can
// place the word in an invocation, as in `echo scan "$STATUS"` — one word in front of
// `scan`, and one single word after it.
func roomForInvocation(word string, front, back int) bool {
	switch word {
	case "ksail":
		return true
	case "workload":
		return back >= 2 || (front >= 1 && back >= 1)
	case "scan":
		return back >= 2 || front >= 2
	}
	return false
}

// plainOptionName is an option name spelled with no shell syntax at all: no quote, no
// backslash, no expansion. Only such a name is the option the raw text shows.
var plainOptionName = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9-]*$`)

// plainOptionAssignment reports whether tok is `<name>=<value>` with the name, up to
// the first `=`, spelled plainly. The first `=` is necessarily outside any quote,
// because the name before it contains no quote character, so whatever quoting follows
// belongs to the value and cannot change which option this is.
func plainOptionAssignment(tok string) bool {
	name, _, found := strings.Cut(tok, "=")
	return found && plainOptionName.MatchString(name)
}

func undecidableOptionWord(args []string) string {
	// The only option words whose NEXT word is necessarily their value. `--verbose "$X"` or
	// `--framework=nsa "$X"` leave the expansion free to become an option of its own, so
	// the exception is a whitelist of value-taking spellings, not any token beginning `-`.
	outputOption := func(tok string) bool {
		return tok == "-o" || tok == "--output"
	}
	for i, tok := range args {
		// `$` and backticks expand; unquoted `*`, `?` and `[` are pathname expansion, and a
		// file named `--framework` beside `--framewor?` is all it takes to build the flag.
		// Quote-aware: `-o '*.sarif'` and `-o out\*.sarif` are literal file names, not
		// patterns, and refusing them was a false positive.
		expansion := carriesExpansion(tok)
		spelled := bareToken(tok) != tok
		switch {
		case !expansion && !spelled:
			continue
		case !expansion && plainOptionAssignment(tok):
			// `--framework="nsa,mitre"`: the option NAME is spelled plainly and the quoting
			// sits only in its VALUE, with nothing to expand. The shell resolves that at
			// parse time into exactly `--framework=nsa,mitre`, so the option is the one the
			// text names and frameworkArgument reads the value the shell passes (#3585). A
			// quoted or escaped NAME (`--frame"work"=…`) is not this shape and is refused below.
			continue
		case strings.HasPrefix(bareToken(tok), "-") || strings.HasPrefix(tok, "-"):
			return fmt.Sprintf("option word %q is not a plain token", tok)
		case expansion && len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' && i > 0 && outputOption(args[i-1]):
			// The one accepted expansion: a double-quoted value of `-o`/`--output`, which cannot
			// split into further words and is consumed by the option before it.
			continue
		case expansion:
			return fmt.Sprintf("argument %q carries a shell expansion or pattern outside a double-quoted -o/--output value", tok)
		default:
			// A quoted or escaped non-option argument (`'nsa,mitre'`) cannot construct an
			// option word, so it is decidable; frameworkTokens still refuses it as a value.
			continue
		}
	}
	return ""
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
