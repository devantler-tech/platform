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

// Only this literal subset can be split on whitespace without evaluating shell
// syntax. Complex command strings keep the existing conservative substring rule.
var literalShellWords = regexp.MustCompile(`^[a-zA-Z0-9_./[:space:]-]+$`)

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

// scanCandidate reports whether a `run:` scalar can invoke the scan at all. Deliberately
// LOOSE: it decides only whether a conditional step is worth refusing, and a false
// positive there costs a diagnosable refusal while a false negative reopens the hole.
//
// It reads the scalar the way the unconditional path does — token by token through
// resolveToken — rather than by raw substring, because a raw `ksail` substring test
// is exactly what a quoted, escaped or expanded spelling walks past: `k"s"ail workload
// scan --framework nsa` in a conditional step used to be skipped as ordinary shell while
// executing a reduced scan the guard never validated. A line whose resolved words
// contain all three of `ksail`, `workload` and `scan` is a candidate, and so is any
// line the undecidable-candidate rule refuses (a plain scan word beside an expansion,
// which covers `k$'s'ail`), so the conditional path fails closed on the same spellings
// the primary path counts or refuses. Quoted prose stays ignored here for the same
// reason it does there.
func scanCandidate(scalar string) bool {
	framed := strings.Contains(scalar, "--framework")
	// A quoted string may span physical lines, and every reading below is per line —
	// so a newline INSIDE quotes is folded to a space first, making the multi-line
	// string one word that fullyQuoted then drops as prose. Comment detection runs on
	// the result, so a line that begins inside a quote is never mistaken for a comment.
	lines := strings.Split(collapseQuotedNewlines(scalar), "\n")
	// SCALAR-WIDE evidence, over the non-comment lines, kept from the raw check this
	// replaced: when the command words are assigned on one line and expanded on the
	// next (`KSAIL=ksail …` then `${KSAIL} ${WORKLOAD} ${SCAN} --framework nsa`) no
	// single line spells the scan, so the per-line rules below see nothing. If the
	// executable text of the block spells all four tokens AND some line expands, the
	// block can invoke the scan and is refused on this deliberately loose path.
	// Built from the QUOTE-AWARE tokens, not the raw text: a fully quoted word is
	// prose and contributes nothing, so `echo "ksail workload scan --framework
	// $STATUS"` is not evidence of a scan, while `${KSAIL}` and `k"s"ail` (resolved to
	// `ksail`) still are.
	var code strings.Builder
	codeExpands := false
	for _, physical := range lines {
		if strings.HasPrefix(strings.TrimSpace(physical), "#") {
			continue
		}
		text, expands := evidenceText(shellFields(physical))
		if expands {
			codeExpands = true
		}
		code.WriteString(text)
		code.WriteByte('\n')
	}
	if text := code.String(); codeExpands && strings.Contains(text, "--framework") &&
		strings.Contains(text, "ksail") && strings.Contains(text, "workload") && strings.Contains(text, "scan") {
		return true
	}
	for i := 0; i < len(lines); i++ {
		// A comment cannot execute anything, and the real workflows annotate their
		// conditional steps with prose that names the scan. Decided on the PHYSICAL
		// line, before any continuation is joined: a backslash at the end of a
		// comment does not continue it, so the next physical line still executes —
		// joining first would fold that line into the comment and skip it.
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			continue
		}
		// A backslash-newline continues the command on the next physical line, so
		// the three scan words can be split across lines; join them before reading.
		// PARITY, via continuesLine, exactly as the unconditional path joins: only an
		// ODD run of trailing backslashes continues the line. `strings.HasSuffix` is
		// true for either parity, so a line ending in an EVEN run folded the lines
		// after it into itself, and an unmatched quote carried in from a comment then
		// made the scan one quoted fragment — no scan word seen, the conditional step
		// skipped as ordinary shell, and the reduced scan it really runs unvalidated.
		logical := lines[i]
		for continuesLine(logical) && i+1 < len(lines) {
			head := strings.TrimSuffix(logical, "\\")
			// bash removes the backslash-newline and reads on: when the continued
			// text begins a word and the next physical line starts with `#`, that
			// `#` opens a comment — `echo \` then `# x \` is `echo # x \`, and the
			// backslash inside the comment continues nothing. The logical line
			// ends here, the comment line is consumed, and the line after it is
			// read on its own. `foo\` then `#bar` is `foo#bar`, not a comment.
			if strings.HasPrefix(strings.TrimSpace(lines[i+1]), "#") && endsAtWordStart(head) {
				logical = head
				i++
				break
			}
			i++
			// DIRECT concatenation, no inserted separator: bash removes the
			// backslash-newline pair and inserts NOTHING, so `k\` + `sail …` is the
			// word `ksail`. Joining with a space reconstructed `k sail`, which carries
			// no scan word, so the step was skipped. It is faithful the other way too:
			// `k\` before an INDENTED `sail` really is `k   sail` to bash, which runs
			// `k` — so that one correctly stays undetected rather than being a gap.
			logical = head + lines[i]
		}
		line := logical
		fields := shellFieldsAcrossSeparators(line)
		if len(fields) == 0 {
			continue
		}
		// COMMAND POSITION, the one thing the text key cannot see. A line whose command
		// word is an expansion resolves only when the shell runs, so it cannot be shown
		// scan-free — `K=ks; K+=ail` … then `"$K" "$W" "$S" "$F" nsa` spells no key
		// substring anywhere while bash executes `ksail workload scan --framework nsa`.
		//
		// `fields[0]` is NOT the command word in general: a leading assignment or
		// redirection precedes it (`OUT=x "$K" …`), and a separator opens a further
		// command position (`true; "$K" …`). Both were measured slipping past a
		// fields[0]-only test, so every command position on the line is judged.
		if expandedCommandWord(line) {
			return true
		}
		// The three words, WITHIN one command rather than anywhere in the scalar.
		// Scalar-wide they combined `ksail` from a `ksail version` line with `workload`
		// and `scan` from a `trivy scan --input workload.yaml` line and refused a step
		// that invokes no Kubescape scan.
		if strings.Contains(line, "ksail") && strings.Contains(line, "workload") &&
			strings.Contains(line, "scan") {
			return true
		}
		words := map[string]bool{}
		for _, f := range fields {
			if word, fragment := resolveToken(f); !fragment {
				words[word] = true
			}
		}
		if framed && words["ksail"] && words["workload"] && words["scan"] {
			return true
		}
		if undecidableScanCandidate(fields) != "" {
			return true
		}
		// A shell interpreter handed the scan as a STRING (`bash -c '…'`, `eval '…'`)
		// runs text no per-token rule can read, and every reading above is per token:
		// the program is one fully quoted word, so resolveToken takes it for prose and
		// no scan word is ever seen. The unconditional path refuses this shape, but a
		// conditional step never reaches that path — it is screened here and dropped —
		// so without the same test a conditional `bash -c 'ksail workload scan …'` is
		// skipped as ordinary shell while the reduced scan it runs overwrites the SARIF.
		if undecidableShellString(fields) != "" {
			return true
		}
		// Every command word expanded at once (`${ksail} ${workload} ${scan}`): no
		// token resolves to a plain scan word, so neither rule above sees it. The
		// unquoted text still spells the scan inside the expansions, and the line
		// expands, which is enough to refuse on the loose path this function serves.
		if text, expands := evidenceText(fields); expands && strings.Contains(text, "ksail") &&
			strings.Contains(text, "workload") && strings.Contains(text, "scan") {
			return true
		}
	}
	// THE INVERSION, and the reason this function stops growing.
	//
	// Everything above tries to RECOGNISE an invocation, and that is a blacklist of
	// spellings. Nine review rounds each found another one it could not read: a prefix,
	// a shell string, an even backslash run, a command word split across a
	// continuation, a scan glued to a separator, a subshell, an alias body, a
	// path-qualified command word, and a step whose shell is not bash at all. Each fix
	// closed exactly one spelling. Nothing suggests the list was ever going to end.
	//
	// For a CONDITIONAL step the question was never "is this a scan". The runner
	// decides whether the step executes before any shell starts, so the only safe
	// question is "can this be shown NOT to run one" — which is a whitelist. Refuse any
	// conditional scalar whose raw text names the scan at all, whatever the spelling,
	// whatever the shell, quoted or not.
	//
	// The cost is that a conditional step MENTIONING the scan — prose, a comment, an
	// echoed diagnostic — is now refused too. That is deliberate and cheap: no real
	// scan step carries an `if:`, the refusal names the step, and the fix is to drop
	// the `if:` or not to spell the scan there. The precision the unconditional path
	// can afford comes from parsing the shell completely; this screen does not, so it
	// buys its safety with a false positive instead.
	return mentionsScanText(scalar)
}

// expandedCommandWord reports whether any COMMAND POSITION on one logical line holds a
// token carrying a shell expansion. Such a word resolves only when the shell runs, so a
// conditional line containing one cannot be shown scan-free.
//
// Command positions are the start of the line and everything after an unquoted
// separator; a leading assignment (`OUT=x`) or redirection (`>/dev/null`) precedes the
// command word rather than being it. Only the command word is judged, so an ordinary
// `echo "$FOO"` or `cp "$SRC" "$DST"` — literal command, expanded arguments — is
// untouched.
func expandedCommandWord(line string) bool {
	atCommand := true
	skipNext := false
	sawWrapper := false
	for _, tok := range shellFieldsWithSeparators(line) {
		if skipNext {
			skipNext = false
			continue
		}
		if isShellSeparator(tok) {
			atCommand = true
			sawWrapper = false
			continue
		}
		if !atCommand {
			continue
		}
		if isAssignmentWord(tok) {
			continue
		}
		if op, alone := redirectionWord(tok); op {
			skipNext = alone
			continue
		}
		if commandNameUndetermined(tok) {
			return true
		}
		// An EXECUTION WRAPPER's operand is the real command word: `command "$K" …`
		// runs whatever `$K` expands to. Treating the literal wrapper as the command
		// word ended the walk here and let an assembled scan through. Its own options
		// are skipped as options, and a leading numeric operand (`timeout 30 cmd`,
		// `nice 10 cmd`) belongs to the wrapper rather than being the command.
		if isExecWrapper(bareToken(tok)) {
			sawWrapper = true
			continue
		}
		if sawWrapper && isBareNumber(tok) {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}
		atCommand = false
	}
	return false
}

// commandNameUndetermined reports whether the NAME the shell would execute is unknown.
// An expanded DIRECTORY with a literal final segment still fixes the name —
// `"${RUNNER_TEMP}/kubectl"` runs kubectl however the directory expands, and refusing it
// blocked a legitimate step — so only an expansion in that final segment leaves the
// command unknown. `"$K"` and `"/usr/bin/$K"` still refuse.
func commandNameUndetermined(tok string) bool {
	return carriesExpansion(tok[strings.LastIndex(tok, "/")+1:])
}

// isExecWrapper names commands whose OPERAND is another command. A list is acceptable
// here in a way a list of scan spellings was not: wrappers are a small, stable set, and
// a missed one costs a missed refusal on an assembled command rather than reopening
// every spelling of an ordinary invocation.
func isExecWrapper(word string) bool {
	switch word[strings.LastIndex(word, "/")+1:] {
	case "command", "builtin", "exec", "nohup", "nice", "ionice", "setsid",
		"stdbuf", "time", "timeout", "env", "sudo", "doas", "chroot", "unbuffer":
		return true
	}
	return false
}

// isBareNumber reports a token that is only digits, optionally with a single-letter
// unit — a wrapper's own operand (`timeout 30`, `timeout 5s`), never a command name.
func isBareNumber(tok string) bool {
	if tok == "" {
		return false
	}
	digits := 0
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case i == len(tok)-1 && ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')):
		default:
			return false
		}
	}
	return digits > 0
}

func isShellSeparator(tok string) bool {
	for i := 0; i < len(tok); i++ {
		if tok[i] != ';' && tok[i] != '&' && tok[i] != '|' {
			return false
		}
	}
	return len(tok) > 0
}

// isAssignmentWord reports a `NAME=` or `NAME+=` prefix, which precedes the command word.
func isAssignmentWord(tok string) bool {
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
		case i > 0 && c >= '0' && c <= '9':
		case i > 0 && c == '+' && i+1 < len(tok) && tok[i+1] == '=':
			return true
		case i > 0 && c == '=':
			return true
		default:
			return false
		}
	}
	return false
}

// redirectionWord reports whether the token is a redirection, and whether the operator
// stands ALONE so its target is the next token (`> file`) rather than attached (`>file`).
func redirectionWord(tok string) (isRedirection, operatorAlone bool) {
	i := 0
	for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
		i++
	}
	if i >= len(tok) || (tok[i] != '<' && tok[i] != '>') {
		return false, false
	}
	for i < len(tok) && (tok[i] == '<' || tok[i] == '>' || tok[i] == '&' || tok[i] == '-') {
		i++
	}
	return true, i == len(tok)
}

// shellFieldsWithSeparators is the separator-splitting tokeniser, but it EMITS each
// unquoted separator as its own token so command positions can be located. Kept
// separate from shellFieldsAcrossSeparators so the rules consuming that one see exactly
// the fields they saw before.
func shellFieldsWithSeparators(text string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\\' && !inSingle:
			cur.WriteByte(c)
			if i+1 < len(text) {
				i++
				cur.WriteByte(text[i])
			}
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case (c == ';' || c == '&' || c == '|') && !inSingle && !inDouble:
			flush()
			out = append(out, string(c))
		case (c == ' ' || c == '\t' || c == '\n' || c == '\r') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// mentionsScanText is the SCALAR-WIDE half of the inversion's key: `--framework`, which
// every spelling of the invocation carries and which no reasonable unrelated command
// does. The three-word test lives per LINE in scanCandidate instead — scalar-wide it
// combined `ksail` from a `ksail version` line with `workload` and `scan` from a
// `trivy scan --input workload.yaml` line and refused a step invoking no scan.
//
// Deliberately NOT keyed on a bare `ksail`: the real ci.yaml runs `shellcheck
// .github/scripts/setup-ksail.sh` inside a CONDITIONAL step, and refusing that would
// reject the very workflow this guard exists to validate — measured, it failed all
// three real-workflow tests.
func mentionsScanText(text string) bool {
	return strings.Contains(text, "--framework")
}

// evidenceText renders the words of one line that could take part in an invocation —
// every token that is not a fully quoted string, resolved through resolveToken — and
// reports whether any token on the line expands. A fully quoted word is an argument,
// never a command, so prose such as `echo "ksail workload scan --framework $STATUS"`
// contributes only `echo`.
func evidenceText(fields []string) (string, bool) {
	var text strings.Builder
	expands := false
	for _, f := range fields {
		if carriesExpansion(f) {
			expands = true
		}
		if fullyQuoted(f) {
			continue
		}
		word, _ := resolveToken(f)
		text.WriteString(word)
		text.WriteByte(' ')
	}
	return text.String(), expands
}

// endsAtWordStart reports whether text ending here leaves the shell at the start of
// a word: empty, or ending in whitespace or an unquoted metacharacter — the position
// in which a following `#` opens a comment.
func endsAtWordStart(head string) bool {
	if head == "" {
		return true
	}
	last := head[len(head)-1]
	return last == ' ' || last == '\t' || strings.IndexByte(";|&()<>", last) >= 0
}

// collapseQuotedNewlines replaces every newline that falls inside a single- or
// double-quoted string with a space, tracking quote state and backslash escapes the way
// shellFields does, so a quoted string that spans physical lines reads as one word on
// one line. Newlines outside quotes are kept, so line structure survives.
//
// A backslash-newline pair inside double quotes is REMOVED, as bash removes it before it
// builds the quoted argument: `echo "ksail workload \` continued by `scan --framework $X"`
// is one printed string, and leaving the newline in place would split it into two physical
// lines that together spell the scan. Outside quotes the pair is kept, because
// scanCandidate joins unquoted continuations itself on the PHYSICAL line — after deciding
// whether that line is a comment, which a backslash does not continue.
//
// A shell COMMENT — a `#` that begins a word outside quotes — runs to its physical
// newline and opens no quote, whatever it contains. It is copied through untouched
// and its newline is kept, so an unmatched quote inside a comment cannot swallow the
// command on the next line into the comment (the fail-open a quote-state-only fold
// has). A `#` inside a quoted string, or in the middle of a word, is not a comment.
func collapseQuotedNewlines(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	inSingle, inDouble := false, false
	// True when the next byte would begin a shell word: at the start of the text and
	// after unescaped whitespace. Only a `#` in that position opens a comment.
	wordStart := true
	for i := 0; i < len(text); i++ {
		c := text[i]
		atWordStart := wordStart
		wordStart = false
		switch {
		case c == '#' && atWordStart && !inSingle && !inDouble:
			for i < len(text) && text[i] != '\n' {
				out.WriteByte(text[i])
				i++
			}
			// Leave the newline to the next iteration: outside quotes it is kept.
			i--
			wordStart = true
		case c == '\\' && inDouble && i+1 < len(text) && text[i+1] == '\n':
			i++
		case c == '\\' && !inSingle:
			out.WriteByte(c)
			if i+1 < len(text) {
				i++
				out.WriteByte(text[i])
				// An unquoted backslash-newline is removed by bash before the next
				// line is read, so the next byte continues the word the backslash
				// ended: `echo \` then `# x` is `echo # x` (a comment), while
				// `foo\` then `#bar` is `foo#bar` (not one). The pair itself is kept
				// here for scanCandidate's join; only the word-start state carries.
				if text[i] == '\n' && !inDouble {
					wordStart = atWordStart
				}
			}
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			out.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			out.WriteByte(c)
		case c == '\n' && (inSingle || inDouble):
			out.WriteByte(' ')
		default:
			out.WriteByte(c)
			// Whitespace ends a word everywhere; the shell metacharacters end one
			// outside quotes, so `true;# x` and `a|# x` open a comment with no space.
			if c == ' ' || c == '\t' || c == '\n' {
				wordStart = true
			} else if !inSingle && !inDouble && strings.IndexByte(";|&()<>", c) >= 0 {
				wordStart = true
			}
		}
	}
	return out.String()
}

// fullyQuoted reports whether a whole word is wrapped in one pair of matching quotes —
// the shape a multi-word string takes once shellFields keeps it together. Such a word is
// an argument, never a command, so it is prose to the loose conditional screen.
func fullyQuoted(tok string) bool {
	if len(tok) < 2 {
		return false
	}
	first, last := tok[0], tok[len(tok)-1]
	return (first == '\'' || first == '"') && last == first
}

// shellFields splits one command line into words the way the shell does: on
// unquoted whitespace only, keeping quote characters and backslash escapes in the
// word so resolveToken reads them exactly as before. `strings.Fields` split a quoted
// argument at every space, so `echo "about ksail workload scan --framework nsa"`
// arrived as separate `ksail`, `workload` and `scan` words and read as a prefixed
// invocation, and `-o "${RUNNER_TEMP}/kubescape report.sarif"` lost the one accepted
// expansion shape because neither half was a whole double-quoted word. An unclosed
// quote runs to the end of the line as one word, which resolveToken then reports as a
// fragment.
func shellFields(text string) []string { return shellFieldsSplit(text, false) }

// shellFieldsAcrossSeparators is shellFields plus a break at an UNQUOTED shell
// separator, so `true;ksail workload scan …` yields `ksail` as its own word. Bash runs
// that scan, but the plain splitter keeps `true;ksail` as one token, so no token
// resolves to a scan word and the conditional screen skipped the step. Only the LOOSE
// conditional screen uses this: it may over-split an exotic line, and a false positive
// there costs a diagnosable refusal while a false negative reopens the hole.
//
// A separator inside quotes is not a command boundary, so `echo 'a;ksail workload
// scan'` stays one quoted word and remains prose — the same treatment the plain
// splitter gives it. An escaped `\;` likewise stays in the word.
func shellFieldsAcrossSeparators(text string) []string { return shellFieldsSplit(text, true) }

func shellFieldsSplit(text string, breakSeparators bool) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\\' && !inSingle:
			cur.WriteByte(c)
			if i+1 < len(text) {
				i++
				cur.WriteByte(text[i])
			}
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case breakSeparators && (c == ';' || c == '&' || c == '|') && !inSingle && !inDouble:
			flush()
		case (c == ' ' || c == '\t' || c == '\n' || c == '\r') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
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

// undecidableShellString names an `eval`, or a shell interpreter given a `-c`-style
// option, whose following arguments name a scan word — text the interpreter will run
// as code, which no per-token rule can read — or returns "" when there is none.
// The interpreter is recognised by its base name so `/bin/sh -c` counts; the option
// test is any dash-option containing `c` (`-c`, `-ec`, `-lc`). Text with no scan word
// is ordinary shell: `bash -c 'echo hello'` is not this guard's business.
func undecidableShellString(fields []string) string {
	for i, f := range fields {
		word := bareToken(f)
		base := word[strings.LastIndex(word, "/")+1:]
		executes := word == "eval"
		// For an interpreter, ONLY the operand right after the `-c`-style option is
		// executed as code; every later field populates $0, $1 … So
		// `bash -c 'printf "%s\n" "$0"' scan-report` runs a printf, not a scan, and
		// scanning all following fields refused it — a false positive on an ordinary
		// step. `eval` is different: it concatenates ALL its arguments and runs the
		// result, so there every following field is code. -1 means "not narrowed".
		operand := -1
		if !executes {
			switch base {
			case "bash", "sh", "dash", "zsh", "ksh":
				// Every leading option is read, not only the first: `bash -e -c '…'`
				// and `bash --noprofile -c '…'` execute their string exactly as
				// `bash -c` does. A short cluster containing `c` (`-c`, `-ec`, `-lc`)
				// is the command-string option. Options such as `-o` consume the next
				// word, so skip that operand before looking for more options; otherwise
				// it would be mistaken for the script path that ends the scan.
				skipOperand := false
				for j := i + 1; j < len(fields); j++ {
					if skipOperand {
						skipOperand = false
						continue
					}
					opt := bareToken(fields[j])
					if !strings.HasPrefix(opt, "-") || opt == "-" || opt == "--" {
						break
					}
					if !strings.HasPrefix(opt, "--") && strings.Contains(opt, "c") {
						executes = true
						operand = j + 1
						break
					}
					if opt == "-o" || (base == "bash" && (opt == "-O" || opt == "--init-file" || opt == "--rcfile")) {
						skipOperand = true
					}
				}
			case "env":
				// GNU `env -S <string>` splits the string into arguments and executes
				// it, so it is a shell-string form exactly like `bash -c` even though
				// env is not a shell. `--split-string=<string>` attaches the code to
				// the option word itself.
				for j := i + 1; j < len(fields); j++ {
					opt := bareToken(fields[j])
					if opt == "-S" || opt == "--split-string" {
						executes = true
						operand = j + 1
						break
					}
					if strings.HasPrefix(opt, "--split-string=") || (strings.HasPrefix(opt, "-S") && len(opt) > 2) {
						executes = true
						operand = j
						break
					}
					if !strings.HasPrefix(opt, "-") || opt == "-" || opt == "--" {
						break
					}
				}
			}
		}
		if !executes {
			continue
		}
		code := fields[i+1:]
		if operand >= 0 {
			if operand >= len(fields) {
				continue
			}
			code = fields[operand : operand+1]
		}
		for _, arg := range code {
			if literal := bareToken(arg); literalShellWords.MatchString(literal) {
				// A filename or identifier containing "scan" is not the word scan.
				// Narrow only literal text: quotes, expansions, operators and other
				// shell syntax inside the command operand still take the rule below.
				found := false
				for _, token := range strings.Fields(literal) {
					switch filepath.Base(token) {
					case "ksail", "workload", "scan":
						found = true
					}
				}
				if !found {
					continue
				}
			}
			if strings.Contains(arg, "ksail") || strings.Contains(arg, "workload") || strings.Contains(arg, "scan") {
				return fmt.Sprintf("%q executes its argument as shell code and that argument names a scan word", word)
			}
		}
	}
	return ""
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
			// matches at run time, so they are as undecidable as `$`. Quote-aware, so a
			// single-quoted or escaped character stays the literal it is.
			if carriesExpansion(f) {
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
// undecidableScanCandidate reports why a line that is not a plainly spelled primary
// invocation still cannot be read as ordinary shell: it names at least one scan word
// (`ksail`, `workload` or `scan`) as a plain, fully resolved token AND carries a shell
// expansion or pathname pattern in some token. Keyed on the plain word rather than on a
// literal `--framework` or a resolvable `workload scan` pair, because each of those
// anchors can be constructed away (`work${LOAD}`, `--frame${SUFFIX}`) while the line
// still executes a scan. A quoted fragment (`"ksail`) is not a plain word, so quoted
// prose stays readable; an empty reason means the line is decidable.
func undecidableScanCandidate(fields []string) string {
	// TOKENS INSIDE A MULTI-WORD QUOTED STRING ARE NOT PLAIN WORDS. resolveToken flags only
	// the token that opens or closes the string; the words between them resolve as plain
	// (`echo "ksail workload scan finished: $STATUS"` puts `workload` and `scan` there), so
	// the quote state is tracked across the line and everything inside it is skipped —
	// the quoting opt-out the messages name must keep working when the prose carries a
	// variable. A balanced token (`"$X"`, `--frame"work"`) is outside any such string.
	inQuote := false
	scanWord := ""
	for _, f := range fields {
		word, fragment := resolveToken(f)
		if fragment {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch word {
		case "ksail", "workload", "scan":
			if scanWord == "" {
				scanWord = f
			}
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
				return true
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
				return true
			}
		}
	}
	return false
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
			// Split on UNQUOTED whitespace only (shellFields): a quoted argument is one
			// word, so quoted prose cannot read as a prefixed invocation and a quoted
			// -o value containing a space keeps its accepted shape.
			fields := shellFields(segment.text)
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
				// ANY PLAIN SCAN WORD BESIDE ANY EXPANSION IS REFUSED, before either anchor below
				// is consulted. Both anchors can be constructed away at once: `ksail work${LOAD}
				// scan --frame${SUFFIX} nsa` carries no literal `--framework` and no resolvable
				// `workload scan` pair, so it fell between the two branches while executing a
				// scan the guard never read. The whitelist is therefore keyed on the one thing
				// a constructed invocation cannot hide — the plain scan word it still needs —
				// and refuses the line whenever any other token expands, whatever the rest
				// spells. The residual is a line that expands all three words, which no lexical
				// test can see. Unquoted prose that names a scan word beside a variable is
				// refused too, and the message names the quoting opt-out.
				// A COMMAND STRING HANDED TO AN EXECUTING SHELL IS ONE QUOTED ARGUMENT, so the
				// consecutive-token rules below never see the invocation inside it, while
				// `bash -c '…'`, `sh -c "…"` and `eval '…'` execute it all the same. The quoted-
				// prose opt-out does not apply to text an interpreter is about to run: refused
				// whenever that argument names a scan word.
				if reason := undecidableShellString(fields); reason != "" {
					return nil, fmt.Errorf(
						"a command string handed to an executing shell names a scan word, so whether it invokes `ksail workload scan --framework` is not decidable from the text — %s: %q. The interpreter runs that string as code, so a reduced scan inside it executes while the guard reads it as an argument. Invoke the scan as a plain command, never through `eval` or `<shell> -c`. See #3338",
						reason, segment.text)
				}
				// EVERY COMMAND WORD EXPANDED AT ONCE on a `--framework` line: `"$KSAIL"
				// "$WORKLOAD" "$SCAN" --framework nsa` names no plain scan word, so the
				// whitelist below has nothing to key on, and the command-word test only reads
				// the word in front of a resolvable `workload scan` pair. Nothing before the
				// flag is decidable, which is exactly the undecidable case.
				if strings.Contains(segment.text, "--framework") && allExpandedBeforeFramework(fields) {
					return nil, fmt.Errorf(
						"every word before `--framework` is a shell expansion, so the command this line runs is not decidable from the text: %q. Variables assigned earlier in the block can spell `ksail workload scan` here, executing a reduced scan the guard never read. Invoke the scan as the bare words `ksail workload scan`, or quote the text if it is not an invocation. See #3338",
						segment.text)
				}
				if reason := undecidableScanCandidate(fields); reason != "" {
					return nil, fmt.Errorf(
						"a line names `ksail`, `workload` or `scan` plainly beside a shell expansion, so whether it invokes `ksail workload scan --framework` is not decidable from the text — %s: %q. A constructed command or option word can execute a scan while reading as something else, so the executed framework set need not be the validated one. Spell the whole invocation plainly, or quote the text if it is not an invocation. See #3338",
						reason, segment.text)
				}
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
	// A `workload scan` PAIR INSIDE A MULTI-WORD QUOTED STRING IS PROSE, NOT A CANDIDATE.
	// resolveToken flags only the opening and closing tokens as fragments; the words
	// between them resolve plain, so `echo "ksail workload scan finished: $STATUS"` used to
	// yield a pair and then refuse the variable after it — while every message here names
	// quoting as the opt-out. The quote state is tracked across the line instead.
	inQuote := false
	plain := make([]string, len(fields))
	for i, f := range fields {
		word, fragment := resolveToken(f)
		if fragment {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		plain[i] = word
	}
	for i := 0; i+1 < len(fields); i++ {
		if plain[i] == "workload" && plain[i+1] == "scan" {
			return i + 2, true
		}
	}
	return 0, false
}
