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

// Matches `<<` / `<<-` followed by an optionally quoted delimiter word.
var heredocStart = regexp.MustCompile(`<<-?\s*(?:'([^']+)'|"([^"]+)"|([A-Za-z_][A-Za-z0-9_]*))`)

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
		invocations = append(invocations, scanInvocations(scalar)...)
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
	var out []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				if key.Kind == yaml.ScalarNode && key.Value == "run" && value.Kind == yaml.ScalarNode {
					out = append(out, value.Value)
				}
				walk(value)
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	return out, nil
}

// scanInvocations returns the lines of one `run:` scalar that actually INVOKE the
// scan.
//
// TWO FILTERS, CLOSING TWO DIFFERENT AXES.
//
// Shell CONTEXT: heredoc bodies are removed first. A heredoc body line genuinely
// begins with `ksail` while executing nothing, so a decoy body could otherwise
// supply the framework list this guard reads while the real scan ran in a form
// the token filter skips. That was the residual left open by #3057 and is what
// #3060 closes.
//
// Command SHAPE: the surviving line's first token must be exactly `ksail`.
// Anything else — `echo`, `printf`, `env ksail`, a function definition, `:` —
// is not a bare invocation and does not count. The earlier version subtracted
// known decoys instead, a list without an end.
//
// The cost is deliberate: a legitimate future invocation that is NOT a bare
// `ksail` line stops matching and trips the fail-closed path. That is the
// correct direction — the guard refuses to bless a form it cannot read.
func scanInvocations(scalar string) []string {
	var out []string
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

		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		if m := heredocStart.FindStringSubmatch(line); m != nil {
			for _, g := range m[1:] {
				if g != "" {
					heredocDelim = g
					break
				}
			}
			heredocIndented = strings.Contains(m[0], "<<-")
			// The line opening a heredoc is still a command line, so fall
			// through and consider it before skipping the body.
		}

		fields := strings.Fields(trimmed)
		if len(fields) == 0 || fields[0] != "ksail" {
			continue
		}
		if !strings.Contains(trimmed, "workload scan") || !strings.Contains(trimmed, "--framework") {
			continue
		}
		// THE SAME RULE AT LINE GRANULARITY. `ksail ... && ksail ...` on one
		// physical line is one record, and reading the last `--framework` would
		// judge the uploaded analysis on a throwaway's framework set.
		if n := strings.Count(trimmed, "ksail workload scan"); n > 1 {
			// Each chained scan is genuinely a separate invocation, so record
			// them all and let the caller reject the ambiguity. Reading the last
			// --framework would judge the uploaded analysis on a throwaway.
			for i := 0; i < n; i++ {
				out = append(out, trimmed)
			}
			continue
		}
		out = append(out, trimmed)
	}
	return out
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
