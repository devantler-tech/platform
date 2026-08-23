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

// What `<<` opened, as far as this can tell.
//
// A DELIMITER IS A SHELL WORD, NOT AN IDENTIFIER. Matching an identifier read two
// DIFFERENT wrong answers off the same construct, in opposite directions. `<<EOF-1`
// matched the prefix `EOF`, whose terminator never arrives, so the rest of the block
// was swallowed as heredoc body — a false reject. `<<1EOF` and `<<\EOF` matched NO
// branch at all, so no heredoc was recorded and the body was handed to the scanner as
// executable code — a FAIL-OPEN, which let a decoy body supply `nsa,mitre` while the
// only scan that ran covered one framework. Both spellings are valid bash; measured.
//
// So the opener is parsed rather than pattern-matched, and anything unreadable fails
// closed instead of being guessed at — guessing is what produced both bugs.
const (
	heredocUnreadable = iota // a `<<` this cannot parse; the caller fails closed
	heredocOpener            // a real heredoc, delimiter parsed
	heredocHereString        // `<<<`: stdin from a string; no body to skip
)

// parseHeredocOpener reads a heredoc redirection at the START of s, which shellSplit
// has already proven to begin at an unquoted `<<`. Quote removal applies per part, so
// `<<'E'OF`, `<<"E"OF` and `<<E\OF` all terminate on the literal word they spell.
func parseHeredocOpener(s string) (h heredoc, consumed, kind int) {
	if !strings.HasPrefix(s, "<<") {
		return heredoc{}, 0, heredocUnreadable
	}
	i := 2
	// `<<<` is a here-STRING: it consumes a word on this line and swallows no body.
	// Left to the heredoc path it parsed as a heredoc whose delimiter was the quoted
	// word, silently eating the rest of the block.
	if i < len(s) && s[i] == '<' {
		return heredoc{}, 3, heredocHereString
	}
	if i < len(s) && s[i] == '-' {
		h.indented = true
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var delim strings.Builder
	for i < len(s) {
		c := s[i]
		// An unquoted word ends at whitespace or a redirection/list operator.
		if c == ' ' || c == '\t' || strings.IndexByte(";&|<>()", c) >= 0 {
			break
		}
		switch c {
		case '\\':
			if i+1 >= len(s) {
				return heredoc{}, 0, heredocUnreadable
			}
			delim.WriteByte(s[i+1])
			i += 2
		case '\'', '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return heredoc{}, 0, heredocUnreadable
			}
			delim.WriteString(s[i+1 : i+1+j])
			i += j + 2
		default:
			delim.WriteByte(c)
			i++
		}
	}
	if delim.Len() == 0 {
		return heredoc{}, 0, heredocUnreadable
	}
	h.delim = delim.String()
	return h, i, heredocOpener
}

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
		// General reachability of a workflow expression is not decidable here, so only a
		// CONSTANT-FALSE literal is refused: that is the demonstrated bypass and it cannot
		// be a legitimate gate. A non-literal always-false expression (`${{ 1 == 2 }}`)
		// remains open by construction and is tracked separately; this is deliberately a
		// partial mitigation and must not be read as closing the class.
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
			out = append(out, v.Value)
		}
	}
	return out, nil
}

// scanCandidate reports whether a `run:` scalar mentions the scan at all. Deliberately
// LOOSE: it decides only whether a conditional step is worth refusing, and a false
// positive there costs a diagnosable refusal while a false negative reopens the hole.
func scanCandidate(scalar string) bool {
	return strings.Contains(scalar, "ksail") && strings.Contains(scalar, "workload") &&
		strings.Contains(scalar, "scan") && strings.Contains(scalar, "--framework")
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
// One heredoc redirection: its terminator, and whether `<<-` lets that terminator
// be indented.
type heredoc struct {
	delim    string
	indented bool
}

// Returns EVERY heredoc opened on this line, in the order the shell will consume
// their bodies. A single line may carry several — `cat <<'A' <<'B'` — and each
// body in turn is non-executing input. Recording only the first stopped the skip
// at the first terminator and handed the following body to the scanner as code.
func shellSplit(line string) (segments []shellSegment, heredocs []heredoc, unreadable, opensBacktick, opensDollar bool, openQuote byte) {
	var cur strings.Builder
	// PARITY, not presence: a substitution CLOSED on its own line cannot suppress a later
	// line, and rejecting it would block an ordinary `echo `+"`"+`date`+"`"+`. An ODD count leaves one
	// open across the newline, which is the shape that makes the next line undecidable.
	btCount := 0
	// DEPTH across the line for `$(...)`, tracked below.
	dollarDepth := 0
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
		// `$(...)` SPANS LINES exactly as a legacy backtick does, and unlike a backtick
		// it is still a substitution INSIDE double quotes -- which is precisely where the
		// parity rule above never reached it, because the closing `)"` on the next line
		// carries no backtick and the scan's own line begins with `ksail`, so no compound
		// token is seen either.
		//
		// DEPTH, not presence: a substitution CLOSED on its own line cannot suppress a
		// later line, and refusing it would block an ordinary `echo "$(date)"` and turn
		// the guard into a permanent fail-closed. Single quotes make `$(` literal text.
		if !inSingle {
			if c == '$' && i+1 < len(line) && line[i+1] == '(' {
				dollarDepth++
				cur.WriteByte(c)
				i++
				cur.WriteByte(line[i])
				continue
			}
			if c == ')' && dollarDepth > 0 {
				dollarDepth--
				cur.WriteByte(c)
				continue
			}
		}
		if inSingle || inDouble {
			cur.WriteByte(c)
			continue
		}
		// Unquoted from here, so an operator here is a real shell operator.
		// LEGACY BACKTICK SUBSTITUTION SPANS LINES, so what runs inside one is not a fact
		// about its own line: `false &&` before the newline suppresses the command on the
		// next, which the per-line walk read as an unconditional invocation. Recorded for
		// the whole scalar and acted on only if a scan is found, exactly like a compound
		// command — a block that never invokes the scan may use whatever shell it likes.
		if c == '`' {
			btCount++
		}
		if c == '<' && i+1 < len(line) && line[i+1] == '<' {
			h, consumed, kind := parseHeredocOpener(line[i:])
			switch kind {
			case heredocOpener:
				heredocs = append(heredocs, h)
				i += consumed - 1
				continue
			case heredocHereString:
				i += consumed - 1
				continue
			default:
				// Refuse the whole line rather than walk past a redirection whose body
				// boundary is unknown: past it, every following line is of unknown status.
				return segments, heredocs, true, btCount%2 == 1, dollarDepth > 0, 0
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
	return segments, heredocs, false, btCount%2 == 1, dollarDepth > 0, quoteOpen(inSingle, inDouble)
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
// continuesLine reports whether bash joins this physical line to the next one.
//
// An ODD number of trailing backslashes continues the line; an EVEN number is escaped
// backslashes and ends it. Parity, for the same reason the backtick rule uses parity.
func continuesLine(line string) bool {
	n := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

func scanInvocations(scalar string) ([]string, error) {
	var out []string
	var compound string
	// A QUEUE, not a single delimiter: one command line may open several heredocs,
	// and their bodies arrive in the order the delimiters were written. Consuming
	// only the first left every later body being read as executable code.
	var pending []heredoc
	// Recorded for the WHOLE scalar, like compound above: a backtick substitution makes
	// the execution of the lines it spans undecidable from the text.
	var sawBacktick bool
	// Same undecidability, and the form the backtick rule cannot see: `$(` is still a
	// substitution inside double quotes, where the parity walk skips it.
	var sawDollar bool
	// Which quote, if any, a previous physical line left open. Same shape as the heredoc
	// queue above: state the shell carries across lines and a per-line walk cannot see.
	var openQuote byte

	// BASH JOINS a line ending in an unquoted backslash to the next physical line, so an
	// `&&` guard written on one line reaches the command on the next -- and this walk,
	// which treats every physical line as its own command line, could not see it. The
	// scan line then read as an unconditional bare invocation and supplied its full
	// framework list while the only scan that executed covered one framework. Measured
	// against bash, and the validator accepted it.
	//
	// JOINED rather than refused. A `run:` block that invokes the scan may also
	// legitimately continue an unrelated command -- ci.yaml alone carries 29 continued
	// lines -- so a scalar-wide refusal would reject the very workflows this guard
	// validates, and a continued scan invocation was not even being read. Joining puts
	// the real command line in front of the `&&`/`||` rule that already exists, rather
	// than adding a third spelling-specific test beside it.
	//
	// The heredoc skip above stays per PHYSICAL line: a backslash at the end of a
	// heredoc body line is data, not a continuation.
	lines := strings.Split(scalar, "\n")
	for idx := 0; idx < len(lines); idx++ {
		line := lines[idx]
		// A QUOTED ARGUMENT MAY SPAN LINES. Skip the lines that sit INSIDE one, exactly as
		// the heredoc body is skipped above -- they are argument text bash never executes,
		// and reading them as commands let a decoy supply the framework set. Consume up to
		// the closing quote and continue from the remainder of that line, so a real command
		// written after the closing quote is still seen.
		if openQuote != 0 {
			rest, closed := consumeQuoted(line, openQuote)
			if !closed {
				continue
			}
			openQuote = 0
			line = rest
			if strings.TrimSpace(line) == "" {
				continue
			}
		}

		if len(pending) > 0 {
			candidate := line
			if pending[0].indented {
				// TABS ONLY. `<<-` strips leading TABS from the terminator line, never spaces --
				// measured: a space-indented `DOC` does NOT end the heredoc in bash. Trimming
				// spaces too ended the body early here, so the lines bash still treats as
				// non-executing input were read as code and could supply the framework set.
				candidate = strings.TrimLeft(candidate, "\t")
			}
			if strings.TrimRight(candidate, " \t\r") == pending[0].delim {
				pending = pending[1:]
			}
			continue
		}

		// Join before anything reads the line, so every later test sees the command line
		// bash actually runs.
		for continuesLine(line) && idx+1 < len(lines) {
			idx++
			line = strings.TrimSuffix(line, "\\") + lines[idx]
		}

		// The comment goes first, so no later test can be satisfied by text the
		// shell never executes.
		command := stripComment(line)
		if strings.TrimSpace(command) == "" {
			continue
		}

		segments, heredocs, unreadable, backtick, dollar, endQuote := shellSplit(command)
		openQuote = endQuote
		if backtick {
			sawBacktick = true
		}
		if dollar {
			sawDollar = true
		}
		if unreadable {
			return nil, fmt.Errorf(
				"a `<<` redirection in this `run:` block is not in a form this guard can read, so the end of its heredoc body is unknown and every following line has undecidable status: %q. A body read as code would let a non-executing decoy supply the framework list. Use a plain heredoc delimiter. See #2823",
				strings.TrimSpace(command))
		}
		pending = append(pending, heredocs...)

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
	if len(out) > 0 && sawDollar {
		return nil, fmt.Errorf(
			"a scan invocation shares its `run:` block with a `$(...)` command substitution left open across a newline, so whether the scan executes is not decidable from the text: a `false &&` before the newline suppresses it while the line still reads as an unconditional invocation, letting a full framework list stand in for a reduced scan that actually runs. Close the substitution on its own line and invoke the scan as a plain command outside it. See #2823")
	}
	if len(out) > 0 && sawBacktick {
		return nil, fmt.Errorf(
			"a scan invocation shares its `run:` block with a legacy backtick command substitution, whose body spans lines, so whether the scan executes is not decidable from the text: a `false &&` before the newline suppresses it while the line still reads as an unconditional invocation, letting a full framework list stand in for a reduced scan that actually runs. Use $(...) and invoke the scan as a plain command outside it. See #2823")
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

// quoteOpen reports which quote character is still open at the end of a physical line,
// or 0 when the line ends outside a string.
//
// A QUOTED ARGUMENT MAY SPAN LINES, and everything else here reads one line at a time.
// `printf '%s\n' '` followed by a scan on the next line and a closing quote is one
// printf argument to bash -- it PRINTS the scan and never runs it -- while a per-line
// walk read that middle line as an unconditional bare invocation and took its framework
// list. Measured: bash emits the decoy and executes only the reduced scan, and the
// validator accepted the workflow.
func quoteOpen(inSingle, inDouble bool) byte {
	switch {
	case inSingle:
		return '\''
	case inDouble:
		return '"'
	default:
		return 0
	}
}

// consumeQuoted walks a line that begins INSIDE an open quoted string and returns the
// text after the closing quote, plus whether the quote closed on this line.
//
// A single-quoted string interprets nothing, so the first `'` closes it. A double-quoted
// one honours backslash escapes, so an escaped quote does not close it.
func consumeQuoted(line string, quote byte) (string, bool) {
	for i := 0; i < len(line); i++ {
		if quote == '"' && line[i] == '\\' {
			i++
			continue
		}
		if line[i] == quote {
			return line[i+1:], true
		}
	}
	return "", false
}

// constantFalse reports whether a workflow `if:` is a literal that can never be true.
//
// Deliberately narrow. Reachability of a real expression is not decidable here, and
// refusing every conditional job rejects the real `validate` job's legitimate path
// filter -- measured. So only the demonstrated literal forms are refused.
func constantFalse(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.ScalarNode {
		return false
	}
	v := strings.TrimSpace(n.Value)
	v = strings.TrimPrefix(v, "${{")
	v = strings.TrimSuffix(v, "}}")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "'false'", "\"false\"", "0":
		return true
	}
	return false
}
