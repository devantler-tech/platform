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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// Matches `<<` / `<<-` plus an optionally quoted delimiter word, ANCHORED: it is applied
// at a position shellSplit has already proven to be unquoted, never searched over a line.
var heredocStart = regexp.MustCompile(`^<<-?\s*(?:'([^']+)'|"([^"]+)"|([A-Za-z_][A-Za-z0-9_]*))`)

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
	data, err := os.ReadFile(workflow)
	if err != nil {
		return nil, fmt.Errorf("%s could not be read; nothing was validated: %w", workflow, err)
	}

	scalars, err := runScalars(data)
	if err != nil {
		return nil, fmt.Errorf("%s could not be parsed as YAML; nothing was validated: %w", workflow, err)
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
func runScalars(data []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
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
		steps := mappingValue(jobs.Content[i], "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, step := range steps.Content {
			if v := mappingValue(step, "run"); v != nil && v.Kind == yaml.ScalarNode {
				out = append(out, v.Value)
			}
		}
	}
	return out, nil
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

// stripComment removes a trailing shell comment. `#` opens a comment only when
// it BEGINS A WORD and is not quoted, which is the shell's own rule — so
// `--framework nsa#x` keeps its value while `ksail --version # ...` loses the
// remainder. Reading the whole line as one string instead let comment text
// supply the framework set on an unrelated command line.
func stripComment(line string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i]
			}
		}
	}
	return line
}

// shellSegment is one command from a split line, together with whether the shell
// reaches it unconditionally.
type shellSegment struct {
	text string
	// conditional is true when the segment sits after `&&` or `||`, so whether it
	// runs at all depends on another command's exit status. `;`, `&` and `|` do
	// not set it: both sides of those run.
	conditional bool
}

// shellSplit walks ONE command line once, tracking shell quoting, and returns the
// command segments it separates on unquoted control operators — each marked with
// whether the shell reaches it unconditionally — plus the delimiter of an unquoted
// heredoc opener.
//
// THIS REPLACES THREE INDEPENDENT STRING TESTS, and that consolidation is the
// point. Counting the fixed spelling `ksail workload scan` to detect chaining, and
// matching a heredoc opener with a regex over the raw line, both read the text in a
// way the shell does not — so `ksail  workload  scan` (re-spaced) hid a chained
// reduced scan, and a `<<` inside a QUOTED filename such as `-o '<<true'` opened a
// heredoc that swallowed one. Each closed spelling left the class open; this asks
// the quoting question once and answers all of them from the same walk.
func shellSplit(line string) (segments []shellSegment, heredocDelim string, heredocIndented bool) {
	var cur strings.Builder
	var inSingle, inDouble bool
	// The first command on a line is always reached; each operator decides the next.
	conditional := false
	flush := func(nextConditional bool) {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segments = append(segments, shellSegment{text: s, conditional: conditional})
		}
		cur.Reset()
		conditional = nextConditional
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && !inSingle {
			cur.WriteByte(c)
			if i+1 < len(line) {
				i++
				cur.WriteByte(line[i])
			}
			continue
		}
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteByte(c)
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			cur.WriteByte(c)
			continue
		}
		if inSingle || inDouble {
			cur.WriteByte(c)
			continue
		}
		// Unquoted from here, so an operator here is a real shell operator.
		if c == '<' && i+1 < len(line) && line[i+1] == '<' && heredocDelim == "" {
			if m := heredocStart.FindStringSubmatch(line[i:]); m != nil {
				for _, g := range m[1:] {
					if g != "" {
						heredocDelim = g
						break
					}
				}
				heredocIndented = strings.HasPrefix(m[0], "<<-")
				i += len(m[0]) - 1
				continue
			}
		}
		// `;`, `&`, `&&`, `|`, `||` all end the current command. Splitting on the
		// SINGLE character and then consuming a doubled one covers both forms
		// without enumerating spellings — and the DOUBLED forms are exactly the
		// conditional ones, so the same test that consumes them marks what follows.
		if c == ';' || c == '&' || c == '|' {
			doubled := (c == '&' || c == '|') && i+1 < len(line) && line[i+1] == c
			flush(doubled)
			if doubled {
				i++
			}
			continue
		}
		cur.WriteByte(c)
	}
	flush(false)
	return segments, heredocDelim, heredocIndented
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

// scanInvocations returns the commands in one `run:` scalar that actually INVOKE
// the scan — one entry per invocation, so two chained scans are two entries and
// the caller rejects the ambiguity rather than judging the upload on the first.
//
// Shell CONTEXT is closed by shellSplit: heredoc bodies are skipped, and an opener
// only counts when it is genuinely unquoted.
//
// Command SHAPE is closed here: a segment's first three tokens must BE
// `ksail workload scan`. Anything else — `echo`, `env ksail ...`, a comment, a
// quoted argument that merely contains the words — is not a bare invocation.
//
// Shell CONTROL FLOW is closed here too, by REJECTING rather than by evaluating.
// Whether an `&&`/`||` branch is taken depends on a command's exit status, which
// the text does not carry — so a scan behind one is a form this cannot read, and
// counting it as executed is what makes it dangerous: `false && ksail workload
// scan --framework nsa,mitre` never runs, yet it would supply the full framework
// list as the guard's sole evidence while a reduced scan the shape test ignores
// (`env ksail ...`) is what the gate actually executes.
//
// The cost is deliberate: a legitimate future invocation in a form this cannot
// read stops matching and trips the fail-closed path. That is the correct
// direction — the guard refuses to bless a form it cannot read.
func scanInvocations(scalar string) ([]string, error) {
	var out []string
	var compound string
	var heredocDelim string
	var heredocIndented bool

	for _, line := range strings.Split(scalar, "\n") {
		if heredocDelim != "" {
			candidate := line
			if heredocIndented {
				candidate = strings.TrimLeft(candidate, " \t")
			}
			if strings.TrimRight(candidate, " \t\r") == heredocDelim {
				heredocDelim = ""
			}
			continue
		}

		// The comment goes first, so no later test can be satisfied by text the
		// shell never executes.
		command := stripComment(line)
		if strings.TrimSpace(command) == "" {
			continue
		}

		segments, delim, indented := shellSplit(command)
		if delim != "" {
			heredocDelim = delim
			heredocIndented = indented
		}

		for _, segment := range segments {
			// Recorded for the WHOLE scalar, and acted on only if a scan is found below:
			// an ordinary `run:` block that never invokes the scan may use whatever shell
			// it likes.
			if compound == "" {
				compound = compoundToken(segment.text)
			}
			fields := strings.Fields(segment.text)
			if len(fields) < 3 || fields[0] != "ksail" || fields[1] != "workload" || fields[2] != "scan" {
				continue
			}
			if !strings.Contains(segment.text, "--framework") {
				continue
			}
			if segment.conditional {
				return nil, fmt.Errorf(
					"a scan invocation is guarded by `&&` or `||`, so whether it runs depends on another command's exit status: %q. A conditionally executed scan is not evidence of what the gate runs — an always-false guard would let a full framework list stand in for a reduced scan that actually executes. Invoke the scan unconditionally. See #2823",
					segment.text)
			}
			out = append(out, segment.text)
		}
	}
	if len(out) > 0 && compound != "" {
		return nil, fmt.Errorf(
			"a scan invocation shares its `run:` block with the compound command %q, so whether it executes is not decidable from the text. An `if`, loop, `case`, function or group body is skipped without any operator on the scan's own line, which would let an unreachable full framework list stand in for a reduced scan that actually runs. Invoke the scan as a plain command, in a block of plain commands. See #2823",
			compound)
	}
	return out, nil
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
