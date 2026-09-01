package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowsDir is where this repository's GitHub Actions live, relative to this
// package.
const workflowsDir = "../../.github/workflows"

// validatorInvocation is the command that actually runs this gate. A workflow
// only counts as covering a trigger if it runs THIS, not merely if it mentions
// the gate by name.
const validatorInvocation = "go run ./scripts/validate-eks-ci-role-policy"

// isolatedChartNamespaceValidatorInvocation renders the reviewed staged-off
// chart and checks every child namespace. It is a separate command because the
// static Go validator runs before KSail is installed in each workflow.
const isolatedChartNamespaceValidatorInvocation = "bash scripts/tests/test-isolated-chart-namespace-rules.sh"

// workflow is the narrow slice of Actions schema this contract needs.
//
// It is parsed as YAML rather than string-matched on purpose: the workflow
// files describe this very trigger in their comments, so a text search would
// pass on prose alone and keep passing after the trigger itself was deleted.
type workflow struct {
	On   triggerSet     `yaml:"on"`
	Jobs map[string]job `yaml:"jobs"`
}

// job and step are named rather than inlined so the workflow-coverage guards in
// this package share ONE parser. The kubescape baseline guard needs `uses:` and
// `with:` as well as `run:`; giving it a second, near-identical loader would
// duplicate this file's parsing logic and trip the repository's duplication
// gate for no benefit.
type job struct {
	Steps []step `yaml:"steps"`
}

type step struct {
	Run            string     `yaml:"run"`
	Uses           string     `yaml:"uses"`
	TimeoutMinutes int        `yaml:"timeout-minutes"`
	With           stepInputs `yaml:"with"`
}

// stepInputs is the slice of an action's `with:` block these contracts read.
// Actions inputs are heterogenous, so unknown keys are simply ignored.
type stepInputs struct {
	Category string `yaml:"category"`
}

// triggerSet is the `on:` block. GitHub accepts three spellings — a mapping
// (`on: {push: {...}}`), a sequence (`on: [push]`) and a bare scalar
// (`on: push`) — and every workflow in this repository currently uses the
// mapping. Decoding all three anyway keeps this guard failing for the ONE
// reason it exists: a genuinely missing push-to-main gate. A strict mapping-only
// decode would instead abort with a parse error the day someone adds an
// unrelated workflow in shorthand, which reads like the contract broke.
//
// The sequence and scalar forms carry no branch filter, so they yield a trigger
// with no branches — correctly not counting as main coverage on their own.
type triggerSet map[string]triggerSpec

func (t *triggerSet) UnmarshalYAML(value *yaml.Node) error {
	*t = make(triggerSet)
	switch value.Kind {
	case yaml.MappingNode:
		return value.Decode((*map[string]triggerSpec)(t))
	case yaml.SequenceNode:
		var names []string
		if err := value.Decode(&names); err != nil {
			return err
		}
		for _, name := range names {
			(*t)[name] = triggerSpec{}
		}
	case yaml.ScalarNode:
		var name string
		if err := value.Decode(&name); err != nil {
			return err
		}
		(*t)[name] = triggerSpec{}
	}
	return nil
}

// triggerSpec captures a trigger's branch filter. `on:` values are heterogenous
// (`merge_group:` is null, `push:` is a mapping), so unmarshalling is lenient
// and a null trigger simply yields no branches.
type triggerSpec struct {
	Branches []string `yaml:"branches"`
}

func (t *triggerSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return nil
	}
	type rawTrigger triggerSpec
	return value.Decode((*rawTrigger)(t))
}

// endsCommand reports whether b terminates a command, so that the next word
// begins a new one. `&&` and `||` are repeats of these single bytes, so
// treating each byte independently is sufficient here.
func endsCommand(b byte) bool {
	switch b {
	case ';', '&', '|', '\n', '(', ')':
		return true
	}
	return false
}

// endsWord reports whether b terminates a word, so a needle followed by b was
// matched whole rather than as the prefix of a longer token.
func endsWord(b byte) bool {
	switch b {
	case ' ', '\t', '\n', ';', '&', '|', ')', '<', '>':
		return true
	}
	return false
}

// assignmentPrefix returns the length of a leading `NAME=value` word, or 0.
// A command may be preceded by any number of these, and the command word is
// still the one after them.
func assignmentPrefix(s string) int {
	i := 0
	for i < len(s) && (s[i] == '_' ||
		(s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') ||
		(i > 0 && s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 || i >= len(s) || s[i] != '=' {
		return 0
	}
	for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' {
		if s[i] == '\'' || s[i] == '"' {
			return 0 // quoted value: too subtle to skip safely, so decline
		}
		i++
	}
	return i
}

// runsCommand reports whether script EXECUTES needle, rather than merely
// mentioning it.
//
// strings.Contains cannot answer that question. It matches a path named in an
// `echo`, sitting in a shell comment, or handed to a different tool as an
// argument — `shellcheck .github/scripts/setup-ksail.sh` is a live example in
// this repository's own ci.yaml. A guard built on it therefore reports a gate
// as covered when nothing runs it, which is exactly the failure this file's
// header comment says the YAML parse exists to prevent: the parse stops a
// workflow's YAML comments from satisfying the guard, but not a shell comment
// or an echo inside the `run:` block it hands back.
//
// The test is positional rather than a deny-list of inert commands: needle must
// begin at a COMMAND WORD. That admits every executable spelling without
// enumerating the text-emitting builtins (`echo`, `printf`, `:`, a heredoc),
// and it stays correct when a new one is invented. It is deliberately strict —
// an unrecognised wrapper makes a covered gate read as UNCOVERED, failing the
// build loudly, rather than passing an absent gate silently.
func runsCommand(script, needle string) bool {
	if needle == "" {
		return false
	}
	atCommandStart := true
	for i := 0; i < len(script); {
		switch c := script[i]; {
		case c == '\\':
			// An escape consumes the next byte, a line continuation
			// included, and never begins a command word.
			atCommandStart = false
			i += 2
			continue
		case c == '<' && i+1 < len(script) && script[i+1] == '<':
			i = skipHeredoc(script, i)
			atCommandStart = false
			continue
		case c == '\'', c == '"':
			i = skipQuoted(script, i)
			atCommandStart = false
			continue
		case c == ' ', c == '\t':
			// Leading blanks do not end the pending command position.
			i++
			continue
		case endsCommand(c):
			atCommandStart = true
			i++
			continue
		case atCommandStart && c == '#':
			// A comment runs to the newline, which then re-arms the
			// command position on the next iteration.
			for i < len(script) && script[i] != '\n' {
				i++
			}
			continue
		case atCommandStart:
			if strings.HasPrefix(script[i:], needle) {
				rest := script[i+len(needle):]
				if rest == "" || endsWord(rest[0]) {
					return true
				}
			}
			if n := assignmentPrefix(script[i:]); n > 0 {
				i += n // still at a command position
				continue
			}
		}
		atCommandStart = false
		i++
	}
	return false
}

// skipQuoted returns the index just past the quoted run starting at i. An
// unterminated quote consumes the remainder, which keeps the scan total.
func skipQuoted(script string, i int) int {
	quote := script[i]
	for i++; i < len(script); i++ {
		if quote == '"' && script[i] == '\\' {
			i++
			continue
		}
		if script[i] == quote {
			return i + 1
		}
	}
	return len(script)
}

// skipHeredoc returns the index just past a heredoc introduced at i, where
// script[i:i+2] is "<<". A heredoc body is inert text, but every line of it
// starts where a command word would, so without this the body of a
// `cat <<'EOF' ... EOF` block reads as a sequence of commands. A here-string
// ("<<<") introduces no body and is left to the ordinary scan. When the
// terminator is missing the remainder is consumed, which keeps the scan total.
func skipHeredoc(script string, i int) int {
	j := i + 2
	if j < len(script) && script[j] == '<' {
		return i + 2 // here-string, not a heredoc
	}
	stripTabs := false
	if j < len(script) && script[j] == '-' {
		stripTabs = true
		j++
	}
	for j < len(script) && (script[j] == ' ' || script[j] == '\t') {
		j++
	}
	var delim strings.Builder
	for j < len(script) {
		c := script[j]
		if c == '\'' || c == '"' {
			j++ // quoting only disables expansion; the word is the same
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || endsCommand(c) {
			break
		}
		delim.WriteByte(c)
		j++
	}
	if delim.Len() == 0 {
		return i + 2
	}
	// The body starts on the line after the one introducing the heredoc.
	for j < len(script) && script[j] != '\n' {
		j++
	}
	for j < len(script) {
		j++ // step past the newline onto the next line
		start := j
		for j < len(script) && script[j] != '\n' {
			j++
		}
		line := script[start:j]
		if stripTabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == delim.String() {
			return j
		}
		if j >= len(script) {
			break
		}
	}
	return len(script)
}

// runsValidator reports whether any job in the workflow actually executes the
// gate.
func (w workflow) runsValidator() bool {
	for _, job := range w.Jobs {
		for _, step := range job.Steps {
			if runsCommand(step.Run, validatorInvocation) {
				return true
			}
		}
	}
	return false
}

// runsIsolatedChartNamespaceValidator reports whether one job installs KSail
// before executing the rendered-child namespace gate. Keeping both steps in
// the same job matters: setup in another job does not share the runner.
func (w workflow) runsIsolatedChartNamespaceValidator() bool {
	for _, job := range w.Jobs {
		ksailReady := false
		for _, step := range job.Steps {
			if runsCommand(step.Run, ".github/scripts/setup-ksail.sh") {
				ksailReady = true
			}
			if ksailReady && step.TimeoutMinutes == 10 && runsCommand(step.Run, isolatedChartNamespaceValidatorInvocation) {
				return true
			}
		}
	}
	return false
}

// triggersOnPushToMain reports whether the workflow fires on a direct push to
// main. Shared by every push-to-main coverage guard in this package.
func (w workflow) triggersOnPushToMain() bool {
	push, ok := w.On["push"]
	if !ok {
		return false
	}
	for _, branch := range push.Branches {
		if branch == "main" {
			return true
		}
	}
	return false
}

// coversPushToMain reports whether the workflow runs the gate on a direct push
// to main.
func (w workflow) coversPushToMain() bool {
	return w.triggersOnPushToMain() && w.runsValidator()
}

func loadWorkflows(t *testing.T) map[string]workflow {
	t.Helper()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	loaded := make(map[string]workflow)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		contents, readErr := os.ReadFile(filepath.Join(workflowsDir, name)) //nolint:gosec // Fixed repository path.
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		var parsed workflow
		if err := yaml.Unmarshal(contents, &parsed); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		loaded[name] = parsed
	}
	if len(loaded) == 0 {
		t.Fatal("no workflows parsed — the guard would pass vacuously")
	}
	return loaded
}

// TestAuthorizationGateRunsOnPushToMain pins the coverage gap that let
// f89efff4 break main invisibly.
//
// ci.yaml gates on `pull_request` and `merge_group`; cd.yaml is
// `workflow_dispatch`. A direct push to main fires none of them, so main's own
// checks stayed green while the authorization surface was broken, and the
// failure surfaced only on unrelated PRs (whose CI builds the merge result and
// therefore inherits main).
func TestAuthorizationGateRunsOnPushToMain(t *testing.T) {
	workflows := loadWorkflows(t)

	covering := make([]string, 0, 1)
	for name, parsed := range workflows {
		if parsed.coversPushToMain() {
			covering = append(covering, name)
		}
	}

	if len(covering) == 0 {
		t.Fatalf("no workflow runs %q on push to main — a direct push to main can "+
			"break the authorization surface with every check on main green. "+
			"Restore a push-triggered workflow that runs the gate.", validatorInvocation)
	}
}

// TestAuthorizationGateGuardIsNotVacuous proves the guard above can actually
// fail. A coverage assertion that cannot go RED is worse than none: it reports
// the hole as closed forever.
//
// The negative controls perturb exactly the two conditions the guard depends
// on, so each must flip it to false on its own.
func TestAuthorizationGateGuardIsNotVacuous(t *testing.T) {
	covering := workflow{
		On: triggerSet{"push": {Branches: []string{"main"}}},
		Jobs: map[string]job{
			"gate": {Steps: []step{{Run: validatorInvocation + " ."}}},
		},
	}
	if !covering.coversPushToMain() {
		t.Fatal("positive control failed: a push-to-main workflow that runs the gate must count")
	}

	t.Run("wrong branch does not count", func(t *testing.T) {
		perturbed := covering
		perturbed.On = triggerSet{"push": {Branches: []string{"release"}}}
		if perturbed.coversPushToMain() {
			t.Fatal("a push trigger on another branch must not satisfy main coverage")
		}
	})

	t.Run("wrong trigger does not count", func(t *testing.T) {
		perturbed := covering
		perturbed.On = triggerSet{"pull_request": {Branches: []string{"main"}}}
		if perturbed.coversPushToMain() {
			t.Fatal("a pull_request trigger must not satisfy push-to-main coverage — " +
				"that is precisely the gap f89efff4 slipped through")
		}
	})

	t.Run("naming the gate without running it does not count", func(t *testing.T) {
		perturbed := covering
		perturbed.Jobs = map[string]job{
			"gate": {Steps: []step{{Run: "echo validate-eks-ci-role-policy"}}},
		}
		if perturbed.coversPushToMain() {
			t.Fatal("merely mentioning the validator must not count as running it")
		}
	})
}

// TestIsolatedChartNamespaceGateCoversEveryDeploymentRoute prevents the
// reviewed isolated-chart allowlist from outliving the rendered-child proof it
// depends on. These four workflows cover pull requests and merge groups,
// direct pushes to main, the manual production deployment, and the
// disaster-recovery rebuild respectively. Recovery publishes and reconciles a
// revision, so omitting it left a real deployment route ungated while the test
// name claimed otherwise.
func TestIsolatedChartNamespaceGateCoversEveryDeploymentRoute(t *testing.T) {
	workflows := loadWorkflows(t)
	for _, name := range []string{"ci.yaml", "validate-main.yaml", "cd.yaml", "dr-rebuild.yaml"} {
		parsed, ok := workflows[name]
		if !ok {
			t.Errorf("required workflow %s is missing", name)
			continue
		}
		if !parsed.runsIsolatedChartNamespaceValidator() {
			t.Errorf("%s must install KSail and then run %q in the same job", name, isolatedChartNamespaceValidatorInvocation)
		}
	}
}

// TestIsolatedChartNamespaceGateCoverageIsNotVacuous ablates the ordering and
// same-runner properties independently so the deployment-route guard above
// cannot pass on an inert command or setup performed in another job.
func TestIsolatedChartNamespaceGateCoverageIsNotVacuous(t *testing.T) {
	covered := workflow{Jobs: map[string]job{
		"gate": {Steps: []step{
			{Run: ".github/scripts/setup-ksail.sh"},
			{Run: isolatedChartNamespaceValidatorInvocation, TimeoutMinutes: 10},
		}},
	}}
	if !covered.runsIsolatedChartNamespaceValidator() {
		t.Fatal("positive control failed: setup followed by the namespace gate in one job must count")
	}

	t.Run("missing gate", func(t *testing.T) {
		ablated := workflow{Jobs: map[string]job{
			"gate": {Steps: []step{{Run: ".github/scripts/setup-ksail.sh"}}},
		}}
		if ablated.runsIsolatedChartNamespaceValidator() {
			t.Fatal("KSail setup without the namespace gate must not count")
		}
	})

	t.Run("gate before setup", func(t *testing.T) {
		ablated := workflow{Jobs: map[string]job{
			"gate": {Steps: []step{
				{Run: isolatedChartNamespaceValidatorInvocation},
				{Run: ".github/scripts/setup-ksail.sh"},
			}},
		}}
		if ablated.runsIsolatedChartNamespaceValidator() {
			t.Fatal("the namespace gate must not count before KSail is installed")
		}
	})

	t.Run("setup in another job", func(t *testing.T) {
		ablated := workflow{Jobs: map[string]job{
			"setup": {Steps: []step{{Run: ".github/scripts/setup-ksail.sh"}}},
			"gate":  {Steps: []step{{Run: isolatedChartNamespaceValidatorInvocation}}},
		}}
		if ablated.runsIsolatedChartNamespaceValidator() {
			t.Fatal("KSail setup in another runner job must not count")
		}
	})

	t.Run("unbounded render", func(t *testing.T) {
		ablated := workflow{Jobs: map[string]job{
			"gate": {Steps: []step{
				{Run: ".github/scripts/setup-ksail.sh"},
				{Run: isolatedChartNamespaceValidatorInvocation},
			}},
		}}
		if ablated.runsIsolatedChartNamespaceValidator() {
			t.Fatal("an unbounded chart render must not satisfy the namespace gate")
		}
	})
}

// TestRunsCommandRejectsInertMentions is the non-vacuity proof for the
// coverage guards above. Every case here names the gate exactly as an
// executable invocation would, and none of them runs it — so a guard built on
// strings.Contains passes all of them while the gate is absent. The
// `shellcheck` case is not hypothetical: ci.yaml really does pass this script
// to shellcheck as an argument in a step that never executes it.
func TestRunsCommandRejectsInertMentions(t *testing.T) {
	const needle = ".github/scripts/setup-ksail.sh"
	for name, script := range map[string]string{
		"echoed":              "echo .github/scripts/setup-ksail.sh",
		"echoed quoted":       "echo \".github/scripts/setup-ksail.sh\"",
		"echoed single quote": "echo '.github/scripts/setup-ksail.sh'",
		"printed":             "printf '%s\\n' .github/scripts/setup-ksail.sh",
		"whole-line comment":  "# .github/scripts/setup-ksail.sh\ntrue",
		"indented comment":    "  \t# .github/scripts/setup-ksail.sh",
		"comment after cmd":   "true # .github/scripts/setup-ksail.sh",
		"comment after &&":    "true && # .github/scripts/setup-ksail.sh",
		"argument to a tool":  "shellcheck .github/scripts/setup-ksail.sh scripts/tests/test-setup-ksail.sh",
		"inside a longer arg": "cat prefix.github/scripts/setup-ksail.sh",
		"prefix of a token":   ".github/scripts/setup-ksail.sh.bak",
		"heredoc body":        "cat <<'EOF'\n.github/scripts/setup-ksail.sh\nEOF",
	} {
		if runsCommand(script, needle) {
			t.Errorf("%s: reported as executed, but %q only MENTIONS the gate", name, script)
		}
	}
}

// TestRunsCommandAcceptsExecutableForms is the other half: the guard must stay
// true for every spelling a workflow legitimately uses, or tightening it would
// simply move the failure from silent-pass to false-alarm.
func TestRunsCommandAcceptsExecutableForms(t *testing.T) {
	const needle = ".github/scripts/setup-ksail.sh"
	for name, script := range map[string]string{
		"bare":             ".github/scripts/setup-ksail.sh",
		"trailing newline": ".github/scripts/setup-ksail.sh\n",
		"with arguments":   ".github/scripts/setup-ksail.sh --verbose",
		"indented":         "  .github/scripts/setup-ksail.sh",
		"after a newline":  "set -euo pipefail\n.github/scripts/setup-ksail.sh",
		"after &&":         "true && .github/scripts/setup-ksail.sh",
		"after ||":         "false || .github/scripts/setup-ksail.sh",
		"after ;":          "true; .github/scripts/setup-ksail.sh",
		"after a pipe":     "true | .github/scripts/setup-ksail.sh",
		"after a comment":  "# install ksail\n.github/scripts/setup-ksail.sh",
		"env prefix":       "KSAIL_VERSION=1.2.3 .github/scripts/setup-ksail.sh",
		"two env prefixes": "A=1 B=2 .github/scripts/setup-ksail.sh",
		"redirected":       ".github/scripts/setup-ksail.sh >/dev/null",
		"after a subshell": "(true) && .github/scripts/setup-ksail.sh",
		"after an echo":    "echo installing\n.github/scripts/setup-ksail.sh",
	} {
		if !runsCommand(script, needle) {
			t.Errorf("%s: reported as absent, but %q EXECUTES the gate", name, script)
		}
	}
}

// TestRunsCommandMatchesTheOtherGateInvocations keeps the two multi-word
// needles honest: they contain spaces, so a whole-word check has to compare the
// needle as a phrase rather than a single token.
func TestRunsCommandMatchesTheOtherGateInvocations(t *testing.T) {
	if !runsCommand("go run ./scripts/validate-eks-ci-role-policy .", validatorInvocation) {
		t.Error("the real validator invocation was not recognised as executed")
	}
	if runsCommand("echo go run ./scripts/validate-eks-ci-role-policy", validatorInvocation) {
		t.Error("an echoed validator invocation was reported as executed")
	}
	if !runsCommand("bash scripts/tests/test-isolated-chart-namespace-rules.sh",
		isolatedChartNamespaceValidatorInvocation) {
		t.Error("the real isolated-chart invocation was not recognised as executed")
	}
	if runsCommand("# bash scripts/tests/test-isolated-chart-namespace-rules.sh",
		isolatedChartNamespaceValidatorInvocation) {
		t.Error("a commented-out isolated-chart invocation was reported as executed")
	}
}
