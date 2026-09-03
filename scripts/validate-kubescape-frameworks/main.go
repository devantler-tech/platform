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
// stripCommentFrom removes a shell comment from ONE PHYSICAL line, starting in the
// quote state a previous physical line left open and returning the state this line
// leaves open.
//
// The carried state is what makes per-physical-line stripping safe. A `#` inside a
// quoted string that SPANS lines is content, not a comment, so stripping each line
// from a clean state would truncate the command at that `#` and hide whatever
// followed. `open` is 0 outside a string, or the quote byte that is still open.
func stripCommentFrom(line string, open byte) (string, byte) {
	inSingle := open == '\''
	inDouble := open == '"'
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
				// Everything after the `#` is comment, so no quote it contains is open.
				return line[:i], 0
			}
		}
	}
	switch {
	case inSingle:
		return line, '\''
	case inDouble:
		return line, '"'
	}
	return line, 0
}

// shellSegment is one command from a split line, together with whether the shell
// reaches it unconditionally.
type shellSegment struct {
	text string
	// conditional is true when the segment sits after `&&` or `||`, so whether it
	// runs at all depends on another command's exit status. `;`, `&` and `|` do
	// not set it: both sides of those run.
	conditional bool
	// statusMasked is true when the operator ENDING this segment discards its exit
	// status: a single `&` backgrounds it, and a single `|` pipes it so only the
	// last stage's status survives without `pipefail`. This is a DIFFERENT property
	// from `conditional` -- such a command does run, but its failure does not fail
	// the step, so it is not a gate. Counting one as the gate let a backgrounded
	// full-framework scan stand in for a reduced scan that actually decided the
	// step's outcome.
	statusMasked bool
	// orElse is true when the operator ENDING this segment was `||`. Whether that
	// discards this command's failure is a property of the RIGHT side, so it is
	// recorded during the split and resolved into statusMasked afterwards.
	orElse bool
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
	flush := func(nextConditional, maskCurrent bool) {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segments = append(segments, shellSegment{text: s, conditional: conditional, statusMasked: maskCurrent})
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
			// MASKING AND CONDITIONALITY ARE SEPARATE PROPERTIES OF THE SAME OPERATOR.
			// A SINGLE `&` or `|` masks the status of the command it ends; `&&` masks
			// nothing (if the left side fails the right never runs, so the failure is the
			// step's). `||` carries BOTH properties -- it makes what follows conditional
			// AND, under the default `bash -e`, a non-zero left side is not an error at
			// all: the right side runs and ITS status becomes the step's. Whether that
			// discards the failure depends on the right side, which is not known until the
			// next segment is read, so it is RECORDED here and resolved once the line is
			// split. Deciding masking from `doubled` alone read `scan || true` as a gate.
			// ATTRIBUTE THE `||` ONLY TO A SEGMENT THIS FLUSH ACTUALLY APPENDED. flush drops
			// an empty command, so on a line whose `||` follows nothing (`echo x; || true`)
			// the last element is the EARLIER segment and marking it would refuse a command
			// no `||` ended.
			before := len(segments)
			flush(doubled, !doubled && (c == '&' || c == '|'))
			if doubled && c == '|' && len(segments) > before {
				segments[len(segments)-1].orElse = true
			}
			if doubled {
				i++
			}
			continue
		}
		cur.WriteByte(c)
	}
	flush(false, false)
	resolveOrElseMasking(segments)
	return segments, heredocs, false, btCount%2 == 1, dollarDepth > 0, quoteOpen(inSingle, inDouble)
}

// resolveOrElseMasking decides, for each segment ended by `||`, whether that `||`
// discarded its failure.
//
// A WHITELIST, NOT A BLACKLIST, and deliberately tiny. `a || b` runs b exactly when a
// fails and then reports b's status, so a's failure survives only if b is CERTAIN to
// fail too. `|| exit 1` and `|| false` are the shapes a real gate uses to turn a
// tolerated failure back into a hard one, and they are the only ones decidable from
// the text. Everything else masks -- `|| true`, `|| echo ...`, `|| $CMD`, an empty
// right side at end of line -- so an unrecognised spelling fails CLOSED instead of
// arriving as the next round of this bug. `|| exit 0` masks, which is why the exit
// code is read rather than the word `exit` matched.
func resolveOrElseMasking(segments []shellSegment) {
	for i := range segments {
		if !segments[i].orElse {
			continue
		}
		// The right side must both be certain to fail AND have its own status reach the
		// step: `scan || exit 1 &` backgrounds the exit, so nothing re-raises anything.
		if i+1 < len(segments) && provablyFails(segments[i+1].text) && !segments[i+1].statusMasked {
			continue
		}
		segments[i].statusMasked = true
	}
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

func prefixedScan(fields []string) bool {
	for i := 1; i+2 < len(fields); i++ {
		if bareToken(fields[i]) == "ksail" &&
			bareToken(fields[i+1]) == "workload" &&
			bareToken(fields[i+2]) == "scan" {
			return true
		}
	}
	return false
}

// undecidableCommandWord names why a `--framework` line's command word cannot be
// read from the text, or returns "" when it can.
//
// A WHITELIST, not a list of expansion syntaxes. `bareToken` resolves quotes and
// backslashes, and that is the whole decidable set; an ANSI-C quote (`k$'s'ail`),
// a locale quote (`k$"s"ail`), a variable, a `$(...)` or a backtick substitution all
// produce their command word only when the shell runs. Refusing each spelling by
// name is the blacklist this guard exists to avoid, and the one it missed would be
// the bypass. So the rule is what a decidable line LOOKS like: every token before
// `--framework` is free of `$` and backticks, and the word in front of `workload
// scan` resolves to exactly `ksail` — or is a quoted-string fragment, which is
// echoed text rather than an invocation and the accepted opt-out. A path-qualified
// `/usr/local/bin/ksail` is refused by the same test: it executes the scan while
// matching nothing the guard counts.
//
// Only tokens BEFORE `--framework` are held to this. The real invocation carries
// `-o "${RUNNER_TEMP}/kubescape.sarif"` after it, and an expansion in an argument
// does not change which command runs.
func undecidableCommandWord(fields []string) string {
	// THE FLAG IS LOCATED AFTER THE `workload scan` PAIR when one is present. Searching the
	// whole line let a prefix argument mask it: in `env NOTE=--framework k$'s'ail workload
	// scan --framework nsa` the first match is the assignment, every check below then
	// stops short of the command word, and the ANSI-C-expanded `ksail` executes unread.
	start := 0
	if j, ok := workloadScanArgs(fields); ok {
		start = j
	}
	fw := -1
	for i := start; i < len(fields); i++ {
		if strings.Contains(fields[i], "--framework") {
			fw = i
			break
		}
	}
	if fw < 0 {
		return ""
	}
	// `--framework` alone is too weak a trigger: an unrelated tool that takes the flag,
	// or quoted text that merely contains it, invokes no scan and must stay readable.
	// The expansion test therefore fires only when a scan WORD stands before the flag.
	// The residual is a line that expands all three words at once, which no lexical
	// test can see; one plain word is enough for this one to fire.
	scanWord := false
	for _, f := range fields[:fw] {
		switch bareToken(f) {
		case "ksail", "workload", "scan":
			scanWord = true
		}
	}
	if scanWord {
		for _, f := range fields[:fw] {
			// Unquoted `*`, `?` and `[` are pathname expansion: `k?ail` is whatever file
			// matches at run time, so they are as undecidable as `$`.
			if strings.ContainsAny(f, "$`*?[") {
				return fmt.Sprintf("%q carries a shell expansion", f)
			}
		}
	}
	for j := 1; j+1 < fw; j++ {
		if bareToken(fields[j]) != "workload" || bareToken(fields[j+1]) != "scan" {
			continue
		}
		word, fragment := resolveToken(fields[j-1])
		if word != "ksail" && !fragment {
			return fmt.Sprintf("%q in front of `workload scan` is not the bare word ksail", fields[j-1])
		}
	}
	return ""
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
		expansion := strings.ContainsAny(tok, "$`*?[")
		spelled := bareToken(tok) != tok
		switch {
		case !expansion && !spelled:
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
		// Reset per physical line: it describes only THIS line's leading segment.
		closedQuoteRemainder := false
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
			// TEXT AFTER THE CLOSING QUOTE IS STILL PART OF THE COMMAND THAT OPENED IT.
			// Only an operator starts a new command, so the FIRST segment of this
			// remainder is argument text of that earlier command, not something in
			// command position. Reading it as a standalone command let
			// `printf %s 'a` / `b' ksail workload scan --framework nsa,mitre` supply the
			// full list -- measured: bash printed those words as printf ARGUMENTS and
			// never invoked ksail, while only a later reduced scan ran.
			// ...UNLESS AN OPERATOR STARTS THE REMAINDER. `'a` / `b'; ksail ...` really does
			// run the scan -- measured in bash -- so keying purely on "quote just closed"
			// refused a legitimate invocation. Only a remainder whose first character is
			// not a command separator is argument text of the opening command.
			if t := strings.TrimLeft(line, " \t"); t != "" && strings.IndexByte(";&|", t[0]) < 0 {
				closedQuoteRemainder = true
			}
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
			// EXACT MATCH. Bash compares the terminator line to the delimiter exactly;
			// trailing spaces or tabs do NOT end the heredoc. Trimming them ended the body
			// early here, so lines bash still treats as heredoc INPUT were read as code --
			// a `DOC   ` line let a decoy `ksail ... --framework nsa,mitre` inside the body
			// supply the framework set while only a later reduced scan actually ran.
			// Reproduced against bash before this changed. Only a CR is removed, because a
			// CRLF line ending is a file-encoding artifact rather than shell content.
			if strings.TrimSuffix(candidate, "\r") == pending[0].delim {
				pending = pending[1:]
			}
			continue
		}

		// COMMENT FIRST, THEN CONTINUATION -- the other order is a fail-open. A backslash
		// at the end of a shell COMMENT does not continue the line: measured in bash, the
		// comment ends at the newline and the NEXT line executes as its own command.
		// Joining first swallowed that next line into the comment and stripped both, so a
		// real reduced scan written under a backslash-terminated comment became invisible
		// and the guard reported OK on a workflow that scans less than it claims.
		//
		// Quote state is carried across the physical lines, because a `#` inside a string
		// that spans lines is content rather than a comment; stripping each line from a
		// clean state would truncate the command there and hide what followed.
		command, quote := stripCommentFrom(line, openQuote)
		for continuesLine(command) && idx+1 < len(lines) {
			idx++
			var next string
			next, quote = stripCommentFrom(lines[idx], quote)
			command = strings.TrimSuffix(command, "\\") + next
		}
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

		for si, segment := range segments {
			// Recorded for the WHOLE scalar, and acted on only if a scan is found below:
			// an ordinary `run:` block that never invokes the scan may use whatever shell
			// it likes.
			if compound == "" {
				compound = compoundToken(segment.text)
			}
			fields := strings.Fields(segment.text)
			// NORMALISED HERE TOO, not only in prefixedScan below. That sweep starts at field
			// index 1, so a quoted or escaped spelling in COMMAND position (`k"s"ail workload
			// scan`) met a raw comparison and fell through as ordinary shell — neither counted
			// nor refused, while executing all the same. To the shell it is the word `ksail`,
			// so it is the gate and is counted like any other invocation.
			if len(fields) < 3 || bareToken(fields[0]) != "ksail" || bareToken(fields[1]) != "workload" || bareToken(fields[2]) != "scan" {
				// A PREFIXED INVOCATION STILL RUNS. The shape test above requires the scan to be
				// in command position, so `env ksail workload scan ...` — and any wrapper such as
				// `sudo`, `nice` or `time` — falls through it. Alone that fails closed, because the
				// scalar then has no countable invocation at all. PAIRED with a counted invocation
				// it does not: the guard validates the counted one and ignores the one that also
				// executes, so the framework set that runs need not be the set that was checked.
				//
				// Refused rather than read, like every other unreadable form here. A prefix can
				// change the environment the scan runs in, so what it executes is not decidable
				// from the text. Keyed on the three scan tokens, never on the prefix word, so an
				// unrelated `env`/`time` command is still ordinary shell. See #3338.
				//
				// THIS ALSO REFUSES UNQUOTED TEXT THAT ONLY CONTAINS THE TOKENS, such as
				// `echo ksail workload scan --framework nsa`, and that is the decision rather
				// than an oversight: it is token-identical to `env ksail workload scan ...`, so
				// separating them means enumerating the commands that execute their arguments —
				// the prefix blacklist this guard exists to avoid, whose missed spelling would
				// be the bypass. The opt-out is to QUOTE the text, which the message names and
				// TestUnrelatedPrefixedCommandIsStillIgnored pins as the accepted control.
				if strings.Contains(segment.text, "--framework") {
					if reason := undecidableCommandWord(fields); reason != "" {
						return nil, fmt.Errorf(
							"the command word of a `--framework` line is not decidable from the text — %s: %q. A shell expansion or an unrecognised spelling in command position can execute `ksail workload scan` while reading as something else, so the executed framework set need not be the validated one. Invoke the scan as the bare word `ksail` with no expansion before `--framework`, or quote the text if it is not an invocation. See #3338",
							reason, segment.text)
					}
				}
				// KEYED ON THE BARE `workload scan` PAIR, NOT ON A LITERAL `--framework`. The
				// command-word check below fires only when the flag is spelled out, so a line
				// whose command word AND option word are both constructed — `k$'s'ail workload
				// scan --frame${SUFFIX} nsa` — used to fall between the two branches: not counted,
				// not refused, executing a scan the guard never read. Any `workload scan` pair
				// followed by an undecidable option word is refused, whatever stands in front
				// of it; the opt-out for quoted prose is unchanged.
				if j, ok := workloadScanArgs(fields); ok {
					if reason := undecidableOptionWord(fields[j:]); reason != "" {
						return nil, fmt.Errorf(
							"an argument of a `workload scan` candidate is not decidable from the text — %s: %q. A shell expansion or a quoted spelling in an option word can execute `--framework` while reading as something else, so the executed framework set need not be the validated one. Spell every option word plainly, or quote the text if it is not an invocation. See #3338",
							reason, segment.text)
					}
				}
				if prefixedScan(fields) && strings.Contains(segment.text, "--framework") {
					return nil, fmt.Errorf(
						"a `ksail workload scan --framework` invocation is preceded by another command or an environment assignment, so it executes without being validated: %q. Paired with a countable invocation this lets the validated framework set differ from the one that actually runs. Invoke the scan with no prefix, or quote the text if it is not an invocation. See #3338",
						segment.text)
				}
				continue
			}
			// THE OPTION WORDS ARE HELD TO THE SAME WHITELIST AS THE COMMAND WORD, and BEFORE the
			// `--framework` test below: `ksail workload scan --frame${SUFFIX} nsa` carries no literal
			// `--framework`, so it used to fall through that test as an unframed scan while executing
			// exactly the option the guard never read. Refused whether or not the flag is spelled out.
			if reason := undecidableOptionWord(fields[3:]); reason != "" {
				return nil, fmt.Errorf(
					"an argument of a `ksail workload scan` invocation is not decidable from the text — %s: %q. A shell expansion or a quoted spelling in an option word can execute `--framework` while reading as something else, so the executed framework set need not be the validated one. Spell every option word plainly, with any expansion confined to a double-quoted option value. See #3338",
					reason, segment.text)
			}
			if !strings.Contains(segment.text, "--framework") {
				continue
			}
			if segment.conditional {
				return nil, fmt.Errorf(
					"a scan invocation is guarded by `&&` or `||`, so whether it runs depends on another command's exit status: %q. A conditionally executed scan is not evidence of what the gate runs — an always-false guard would let a full framework list stand in for a reduced scan that actually executes. Invoke the scan unconditionally. See #2823",
					segment.text)
			}
			// A RUNNING SCAN IS NOT A GATE IF ITS FAILURE IS DISCARDED. Backgrounding with
			// `&`, or piping with `|` so only the last stage's status survives, lets the
			// scan execute and fail while the step still succeeds. Measured: a decoy
			// `ksail workload scan --framework nsa,mitre &` exited 42 and the step returned
			// success from a later reduced scan, so the full list was credited to a gate
			// that gated nothing. Refused rather than skipped, because silently ignoring it
			// would equally hide a real scan someone backgrounded by mistake.
			// The first segment of a closed multiline-quote remainder is ARGUMENT text of
			// the command that opened the quote, so a scan spelled there never executes.
			// Refused rather than skipped: it looks exactly like a gate to a reader, and
			// silently ignoring it would hide a real scan someone wrote in the wrong place.
			if si == 0 && closedQuoteRemainder {
				return nil, fmt.Errorf(
					"a scan invocation sits after the closing quote of a string that began on an earlier line, so it is an ARGUMENT to that earlier command rather than a command of its own and never executes: %q. A decoy written this way would supply a full framework list while a reduced scan actually runs. Invoke the scan on its own line. See #2823",
					segment.text)
			}
			if segment.statusMasked {
				return nil, fmt.Errorf(
					"a scan invocation's exit status is discarded by the `&`, `|` or `||` that ends it, so its failure does not fail the step: %q. A scan whose failure is ignored is not a gate — it would let a full framework list stand in for a reduced scan that actually decides the outcome. Invoke the scan as a plain command whose status the step sees. See #2823",
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

// workloadScanArgs returns the index of the first token after a bare `workload scan` pair
// and whether one is present. The pair is the weakest evidence this guard acts on: it
// needs no readable command word in front of it, because that word is exactly what an
// expansion hides.
func workloadScanArgs(fields []string) (int, bool) {
	for i := 0; i+1 < len(fields); i++ {
		if bareToken(fields[i]) == "workload" && bareToken(fields[i+1]) == "scan" {
			return i + 2, true
		}
	}
	return 0, false
}
