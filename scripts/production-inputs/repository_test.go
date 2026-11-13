package main

import (
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	workflowPath  = ".github/workflows/ci.yaml"
	manualPath    = ".github/workflows/cd.yaml"
	rebuildPath   = ".github/workflows/dr-rebuild.yaml"
	actionPath    = ".github/actions/deploy-prod/action.yml"
	actionUse     = "./.github/actions/deploy-prod"
	restrictRun   = "go run ./scripts/production-inputs restrict"
	restrictGuard = "inputs.production-inputs-only == 'true'"
	restrictInput = "production-inputs-only"
	signerGate    = "./scripts/guard-publish-workflow-approved-revisions.sh"
	// classifierStep is the whole script of the changes job's classifier step.
	classifierStep = `answer=production=true
checked_out="$(git rev-parse --verify 'HEAD^{commit}' 2>/dev/null || true)"
if [ -n "$QUEUE_HEAD_SHA" ] && [ "$checked_out" = "$QUEUE_HEAD_SHA" ] &&
  git diff --quiet --end-of-options "$QUEUE_BASE_SHA" "$QUEUE_HEAD_SHA" -- scripts/production-inputs go.mod go.sum 2>/dev/null; then
  reported="$(go run ./scripts/production-inputs changed --base "$QUEUE_BASE_SHA" --head "$QUEUE_HEAD_SHA" || true)"
  if [ "$reported" = production=false ]; then
    answer=production=false
  fi
else
  echo '- Production inputs: production=true (the classifier or its Go module files changed, or the queue commits could not be confirmed).' >>"$GITHUB_STEP_SUMMARY"
fi
echo "$answer"
echo "$answer" >>"$GITHUB_OUTPUT"`
)

// repositoryRoot returns the checkout these tests run in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("these tests read the repository they run in: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func readFile(t *testing.T, root, file string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

type step struct {
	ID       string         `yaml:"id"`
	Name     string         `yaml:"name"`
	If       string         `yaml:"if"`
	Uses     string         `yaml:"uses"`
	Run      string         `yaml:"run"`
	Shell    string         `yaml:"shell"`
	Continue any            `yaml:"continue-on-error"`
	With     map[string]any `yaml:"with"`
	Env      map[string]any `yaml:"env"`
}

type workflow struct {
	Jobs map[string]struct {
		Outputs map[string]string `yaml:"outputs"`
		Steps   []step            `yaml:"steps"`
	} `yaml:"jobs"`
}

type action struct {
	Inputs map[string]struct {
		Default  string `yaml:"default"`
		Required bool   `yaml:"required"`
	} `yaml:"inputs"`
	Runs struct {
		Steps []step `yaml:"steps"`
	} `yaml:"runs"`
}

// wiringFault names the first way the workflows and the action fail to run
// the merge-queue deploy, and only that deploy, from the production inputs.
func wiringFault(ci, manual, rebuild, deploy string) string {
	var queue, cd, dr workflow
	var composite action
	for text, target := range map[string]any{ci: &queue, manual: &cd, rebuild: &dr, deploy: &composite} {
		if err := yaml.Unmarshal([]byte(text), target); err != nil {
			return "unparsable: " + err.Error()
		}
	}

	input, declared := composite.Inputs[restrictInput]
	if !declared || input.Default != "false" || input.Required {
		return "the action must declare the restriction as an optional input that is off by default"
	}
	steps := composite.Runs.Steps
	if len(steps) < 4 {
		return "the action has too few steps to hold its gates and the restriction"
	}
	restriction := steps[2]
	if strings.TrimSpace(restriction.Run) != restrictRun || restriction.If != restrictGuard ||
		restriction.Shell != "bash" || restriction.Continue != nil || restriction.Uses != "" || len(restriction.Env) != 0 {
		return "the action's third step must be the plain restriction, run only when the input asks for it"
	}
	for index, other := range steps {
		if index != 2 && (strings.Contains(other.Run, "production-inputs restrict") || strings.Contains(other.If, restrictInput)) {
			return "only the action's third step may restrict the working tree or read its input"
		}
	}
	// Nothing but the two pinned gates may run before the restriction. The
	// signer gate is the one the self-containment test runs in both trees.
	if strings.TrimSpace(steps[0].Run) != signerGate || steps[0].Uses != "" ||
		steps[1].Uses != "" || steps[1].ID != "verify_recovery_source" {
		return "only the signer and recovery gates may precede the restriction"
	}

	calls := func(parsed workflow, job string) []step {
		var found []step
		for _, candidate := range parsed.Jobs[job].Steps {
			if candidate.Uses == actionUse {
				found = append(found, candidate)
			}
		}
		return found
	}
	queued := calls(queue, "deploy-prod")
	if len(queued) != 1 || queued[0].With[restrictInput] != "true" {
		return "the merge-queue deploy must call the action once with the restriction on"
	}
	for name, parsed := range map[string]workflow{"ci": queue, "cd": cd, "dr-rebuild": dr} {
		for job, definition := range parsed.Jobs {
			for _, candidate := range definition.Steps {
				if name == "ci" && job == "deploy-prod" {
					continue
				}
				if _, set := candidate.With[restrictInput]; set {
					return "only the merge-queue deploy may set the restriction: " + name + " " + job
				}
			}
		}
	}

	changes := queue.Jobs["changes"]
	if changes.Outputs["production"] != "${{ steps.production-inputs.outputs.production }}" {
		return "the changes job must export the classifier's answer"
	}
	classified := 0
	for _, candidate := range changes.Steps {
		if candidate.ID != "production-inputs" {
			continue
		}
		classified++
		if candidate.If != "github.event_name == 'merge_group'" || candidate.Continue != nil || candidate.Uses != "" {
			return "the classifier step must be a plain step that runs for every merge group"
		}
		if len(candidate.Env) != 2 ||
			candidate.Env["QUEUE_BASE_SHA"] != "${{ github.event.merge_group.base_sha }}" ||
			candidate.Env["QUEUE_HEAD_SHA"] != "${{ github.event.merge_group.head_sha }}" {
			return "the classifier step must compare exactly the commits the queue event names"
		}
		// The step is short enough to pin whole: every line is part of how it
		// stays true unless the classifier answers an explicit false.
		if strings.TrimSpace(candidate.Run) != classifierStep {
			return "the classifier step must run exactly the pinned script"
		}
	}
	if classified != 1 {
		return "the changes job must classify production inputs exactly once"
	}
	return ""
}

func TestWorkflowsRunOnlyTheQueueDeployFromProductionInputs(t *testing.T) {
	root := repositoryRoot(t)
	ci, manual := readFile(t, root, workflowPath), readFile(t, root, manualPath)
	rebuild, deploy := readFile(t, root, rebuildPath), readFile(t, root, actionPath)
	if fault := wiringFault(ci, manual, rebuild, deploy); fault != "" {
		t.Fatal(fault)
	}

	replace := func(text, old, replacement string) string {
		t.Helper()
		if strings.Count(text, old) != 1 {
			t.Fatalf("the ablation needs exactly one %q", old)
		}
		return strings.Replace(text, old, replacement, 1)
	}
	for name, mutated := range map[string][4]string{
		"the queue deploy stops restricting": {replace(ci, "          production-inputs-only: \"true\"\n", ""), manual, rebuild, deploy},
		"the queue deploy turns it off":      {replace(ci, "production-inputs-only: \"true\"", "production-inputs-only: \"false\""), manual, rebuild, deploy},
		"the restriction becomes optional": {ci, manual, rebuild,
			replace(deploy, "      run: "+restrictRun+"\n", "      run: "+restrictRun+" || true\n")},
		"the restriction is always on": {ci, manual, rebuild, replace(deploy, "      if: "+restrictGuard+"\n", "")},
		"the restriction is on by default": {ci, manual, rebuild,
			replace(deploy, "deploys keep the whole checkout. Needs Go on the runner.\n    required: false\n    default: \"false\"",
				"deploys keep the whole checkout. Needs Go on the runner.\n    required: false\n    default: \"true\"")},
		"the restriction moves after tool setup": {ci, manual, rebuild, func() string {
			var parsed yaml.Node
			if err := yaml.Unmarshal([]byte(deploy), &parsed); err != nil {
				t.Fatal(err)
			}
			steps := mappingValue(mappingValue(parsed.Content[0], "runs"), "steps")
			steps.Content[2], steps.Content[3] = steps.Content[3], steps.Content[2]
			moved, err := yaml.Marshal(&parsed)
			if err != nil {
				t.Fatal(err)
			}
			return string(moved)
		}()},
		"the manual deploy restricts too": {ci, replace(manual, "          recover-orphaned-fence: \"true\"\n",
			"          recover-orphaned-fence: \"true\"\n          production-inputs-only: \"true\"\n"), rebuild, deploy},
		"the classifier starts from false": {replace(ci, "          answer=production=true\n", "          answer=production=false\n"), manual, rebuild, deploy},
		"the classifier may judge its own change": {replace(ci, " -- scripts/production-inputs go.mod go.sum 2>/dev/null; then\n",
			" -- go.mod go.sum 2>/dev/null; then\n"), manual, rebuild, deploy},
		"the checked-out commit is not confirmed": {replace(ci, "          if [ -n \"$QUEUE_HEAD_SHA\" ] && [ \"$checked_out\" = \"$QUEUE_HEAD_SHA\" ] &&\n",
			"          if [ -n \"$QUEUE_HEAD_SHA\" ] &&\n"), manual, rebuild, deploy},
		"any output but true counts as false": {replace(ci, "            if [ \"$reported\" = production=false ]; then\n",
			"            if [ \"$reported\" != production=true ]; then\n"), manual, rebuild, deploy},
		"the base is not the queue's": {replace(ci, "          QUEUE_BASE_SHA: ${{ github.event.merge_group.base_sha }}\n",
			"          QUEUE_BASE_SHA: ${{ github.event.merge_group.head_sha }}\n"), manual, rebuild, deploy},
		"the classifier may be skipped": {replace(ci, "        id: production-inputs\n",
			"        id: production-inputs\n        continue-on-error: true\n"), manual, rebuild, deploy},
		"the classifier runs for pull requests only": {replace(ci, "        id: production-inputs\n        if: github.event_name == 'merge_group'\n",
			"        id: production-inputs\n        if: github.event_name == 'pull_request'\n"), manual, rebuild, deploy},
		"another command replaces the signer gate": {ci, manual, rebuild,
			replace(deploy, "      run: "+signerGate+"\n", "      run: ./scripts/other.sh\n")},
		"the answer is not exported": {replace(ci, "      production: ${{ steps.production-inputs.outputs.production }}\n", ""), manual, rebuild, deploy},
	} {
		if wiringFault(mutated[0], mutated[1], mutated[2], mutated[3]) == "" {
			t.Errorf("%s: the wiring check did not notice", name)
		}
	}
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return &yaml.Node{}
}

// The trace starts from what the deploy job runs. A step that runs something
// else would start a branch the trace never walks.
func TestTraceStartsFromEverythingTheDeployJobRuns(t *testing.T) {
	root := repositoryRoot(t)
	var parsed workflow
	if err := yaml.Unmarshal([]byte(readFile(t, root, workflowPath)), &parsed); err != nil {
		t.Fatal(err)
	}
	steps := parsed.Jobs["deploy-prod"].Steps
	if len(steps) == 0 {
		t.Fatal("the workflow has no deploy job to trace")
	}
	program := regexp.MustCompile(`go run \./(\S+)`)
	local := 0
	for _, current := range steps {
		var named []string
		if strings.HasPrefix(current.Uses, "./") {
			named = append(named, strings.TrimPrefix(current.Uses, "./"))
		}
		for _, match := range program.FindAllStringSubmatch(current.Run, -1) {
			named = append(named, match[1])
		}
		// Any other repository path in an inline command is a branch too.
		for _, match := range literalPath.FindAllStringSubmatch(current.Run, -1) {
			named = append(named, match[1])
		}
		for _, entry := range named {
			local++
			if !contains(deployEntrypoints, path.Clean(entry)) {
				t.Errorf("the deploy job runs %s, which the trace does not start from", entry)
			}
		}
	}
	if local < 2 {
		t.Fatalf("found %d local commands in the deploy job; the test no longer reads it", local)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestTheListCoversEverythingTheDeployCanReach(t *testing.T) {
	root := repositoryRoot(t)
	list, err := parseInputs(readFile(t, root, listPath))
	if err != nil {
		t.Fatal(err)
	}
	files, err := tracked(root)
	if err != nil {
		t.Fatal(err)
	}
	if stale := staleEntries(list, files); len(stale) > 0 {
		t.Errorf("the list names paths the repository does not track: %v", stale)
	}

	traced, missing, err := uncovered(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("the deploy can reach files the list does not hold:\n  %s", strings.Join(missing, "\n  "))
	}
	// The trace must be seeing the deploy: these are read on every run.
	for _, expected := range []string{
		actionPath,
		".github/actions/deploy-prod/publish-platform-manifests/action.yml",
		"scripts/refresh-flux-ghcr-auth.sh",
		"scripts/ghcr-auth-lib.sh",
		"scripts/run-ksail-prod-with-pull-auth.sh",
		"scripts/tests/test-pvc-prune-safety.sh",
		"scripts/verify-merge-group-queued/main.go",
		"scripts/maintain-longhorn-clients/main.go",
		"internal/longhornmigration/pools.go",
		"scripts/publish-workflow-approved-revisions.tsv",
		"ksail.prod.yaml",
		"go.mod",
		listPath,
	} {
		if !contains(traced, expected) {
			t.Errorf("the trace no longer reaches %s", expected)
		}
	}
	for _, file := range traced {
		if strings.HasPrefix(file, "docs/") || strings.HasSuffix(file, "_test.go") {
			t.Errorf("the trace reached %s, which a deploy does not read", file)
		}
	}
}

// The git subcommands that can read tracked content from somewhere other than
// the working tree: history, the index, another checkout or a remote.
var historyCommands = map[string]bool{
	"archive": true, "show": true, "cat-file": true, "worktree": true, "ls-tree": true,
	"restore": true, "checkout": true, "read-tree": true, "grep": true, "diff": true,
	"log": true, "stash": true, "apply": true, "bundle": true, "format-patch": true,
	"blame": true, "clone": true, "fetch": true, "pull": true, "submodule": true,
}

var (
	// git as a command word, an array that holds it ("${git[@]}"), or a
	// wrapper named for it (checkout_git).
	shellGit   = regexp.MustCompile(`(^|[^A-Za-z0-9_./-])git([[:space:]]|$)|git\[@\]|[A-Za-z0-9]_git([[:space:]]|$)`)
	goGit      = regexp.MustCompile(`"git"|\bgit\(`)
	quotedWord = regexp.MustCompile(`"([a-z][a-z-]*)"`)
	bareWord   = regexp.MustCompile(`[a-z][a-z-]*`)
)

// historyReads returns the history-reading git subcommands a file can run.
// It reads whole statements: a shell command continued over several lines,
// and the arguments that follow "git" in a Go call wherever they are written.
func historyReads(file, text string) []string {
	found := map[string]bool{}
	if path.Ext(file) == ".go" {
		for _, at := range goGit.FindAllStringIndex(text, -1) {
			window := text[at[0]:min(len(text), at[0]+400)]
			for _, word := range quotedWord.FindAllStringSubmatch(window, -1) {
				if historyCommands[word[1]] {
					found[word[1]] = true
				}
			}
		}
	} else {
		for _, line := range strings.Split(strings.ReplaceAll(text, "\\\n", " "), "\n") {
			at := shellGit.FindStringIndex(line)
			if at == nil {
				continue
			}
			for _, word := range bareWord.FindAllString(line[at[0]:], -1) {
				if historyCommands[word] {
					found[word] = true
				}
			}
		}
	}
	commands := make([]string, 0, len(found))
	for command := range found {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	return commands
}

func TestHistoryReadsSeeEveryWayToCallGit(t *testing.T) {
	for name, example := range map[string]struct{ file, text, want string }{
		"a plain command":      {"a.sh", "git -C \"$root\" archive \"$sha\" | tar -x\n", "archive"},
		"a continued command":  {"a.sh", "git -C \"$root\" \\\n  show \"$sha:file\"\n", "show"},
		"an array":             {"a.sh", "\"${git[@]}\" cat-file -p \"$object\"\n", "cat-file"},
		"a wrapper":            {"a.sh", "checkout_git diff --quiet \"$@\"\n", "diff"},
		"a split Go call":      {"a.go", "exec.Command(\n\t\"git\",\n\t\"-C\", dir,\n\t\"ls-tree\", \"-r\", sha,\n)\n", "ls-tree"},
		"a Go helper":          {"a.go", "git(repository, \"restore\", \"--source\", sha)\n", "restore"},
		"two commands":         {"a.sh", "git fetch origin\ngit grep -n x \"$sha\"\n", "fetch grep"},
		"no history read":      {"a.sh", "git rev-parse HEAD\ngit status --porcelain\n", ""},
		"a word that is not a": {"a.sh", "echo show the diff in the log\n", ""},
	} {
		if got := strings.Join(historyReads(example.file, example.text), " "); got != example.want {
			t.Errorf("%s: found %q, want %q", name, got, example.want)
		}
	}
}

// A file read out of Git's history is still there after the working tree is
// restricted, so every script and program the deploy can reach is listed here
// with the history-reading git subcommands it runs and what each one reads.
// The entry must match exactly: a new subcommand needs a new review.
func TestReadsFromGitHistoryAreReviewed(t *testing.T) {
	reviewed := map[string]string{
		// diff --name-only between two commits, limited to k8s/: names only,
		// under a listed path.
		"scripts/maintain-longhorn-clients/interruption.go": "diff",
		// cat-file -e checks that a commit and its list exist, show reads the
		// list at two commits, diff --name-only lists the files that differ.
		"scripts/production-inputs/main.go": "cat-file diff show",
		// cat-file -e checks that the base commit exists. archive unpacks the
		// base commit, of which only k8s/providers/hetzner is rendered.
		"scripts/tests/test-pvc-prune-safety.sh": "archive cat-file",
		// diff --quiet compares the working tree with its commit. It runs only
		// in recovery deploys, which keep the whole checkout.
		"scripts/verify-prod-recovery-source.sh": "diff",
	}
	root := repositoryRoot(t)
	traced, err := trace(root)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, file := range traced {
		if !followed(file) {
			continue
		}
		if commands := historyReads(file, code(file, readFile(t, root, file))); len(commands) > 0 {
			found[file] = strings.Join(commands, " ")
		}
	}
	for file, commands := range found {
		if reviewed[file] != commands {
			t.Errorf("%s runs git %q but %q is reviewed: check what each reads from history and record it here", file, commands, reviewed[file])
		}
	}
	for file := range reviewed {
		if _, ok := found[file]; !ok {
			t.Errorf("%s no longer reads from Git's history; remove its entry", file)
		}
	}
}

// The deploy's own tools must parse, resolve what they source and compile in
// a working tree that holds only the list.
func TestTheRestrictedTreeIsSelfContained(t *testing.T) {
	if testing.Short() {
		t.Skip("clones the repository and compiles the deploy programs")
	}
	root := repositoryRoot(t)
	clone := filepath.Join(t.TempDir(), "clone")
	git := func(directory string, arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", directory,
			"-c", "user.name=test", "-c", "user.email=test@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
		return string(output)
	}
	git(root, "clone", "--quiet", "--no-hardlinks", "--no-checkout", root, clone)
	git(clone, "checkout", "--quiet", "--detach", strings.TrimSpace(git(root, "rev-parse", "HEAD")))

	// Carry over work that is not committed yet, so the test judges this tree.
	pending := splitNUL([]byte(git(root, "ls-files", "-z", "--modified", "--others", "--exclude-standard")))
	for _, file := range pending {
		source := filepath.Join(root, filepath.FromSlash(file))
		target := filepath.Join(clone, filepath.FromSlash(file))
		text, err := os.ReadFile(source)
		if os.IsNotExist(err) {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, text, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	if len(pending) > 0 {
		git(clone, "add", "--all")
		git(clone, "commit", "--quiet", "-m", "uncommitted work")
	}

	var report bytes.Buffer
	if err := restrict(clone, &report); err != nil {
		t.Fatal(err)
	}
	t.Log(strings.TrimSpace(report.String()))

	remaining := splitNUL([]byte(git(clone, "ls-files", "-z")))
	var scripts, programs []string
	for _, file := range remaining {
		if _, err := os.Lstat(filepath.Join(clone, filepath.FromSlash(file))); err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(file, ".sh"):
			scripts = append(scripts, file)
		case path.Base(file) == "main.go" && strings.HasPrefix(file, "scripts/"):
			programs = append(programs, "./"+path.Dir(file))
		}
	}
	sort.Strings(programs)
	if len(scripts) < 30 || len(programs) < 5 {
		t.Fatalf("found %d scripts and %d programs in the restricted tree; the test no longer sees the deploy", len(scripts), len(programs))
	}

	for _, script := range scripts {
		if output, err := exec.Command("bash", "-n", filepath.Join(clone, filepath.FromSlash(script))).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %s", script, output)
		}
	}

	// ShellCheck follows every "source" it can resolve and reports the ones it
	// cannot, which is exactly a library the list forgot.
	shellcheck, err := exec.LookPath("shellcheck")
	switch {
	case err == nil:
		command := exec.Command(shellcheck, append([]string{"--external-sources", "--format=gcc", "--include=SC1091,SC1094"}, scripts...)...)
		command.Dir = clone
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("a script sources a file the list does not hold:\n%s", output)
		}
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Error("ShellCheck is required in CI to prove that sourced libraries are listed")
	default:
		t.Log("ShellCheck is not installed; sourced libraries were not checked")
	}

	build := exec.Command("go", append([]string{"build", "-o", os.DevNull}, programs...)...)
	build.Dir = clone
	build.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Errorf("the deploy programs do not compile from the production inputs alone:\n%s", output)
	}

	// The action's first gate runs before the restriction. Prove it gives the
	// same answer afterwards, so nothing it reads is outside the list.
	gate := exec.Command("bash", "scripts/guard-publish-workflow-approved-revisions.sh")
	gate.Dir = clone
	gate.Env = append(os.Environ(), "APPROVED_REVISIONS_ENFORCE=1")
	restricted, err := gate.CombinedOutput()
	if err != nil {
		t.Fatalf("the signer gate fails from the production inputs alone:\n%s", restricted)
	}
	whole := exec.Command("bash", "scripts/guard-publish-workflow-approved-revisions.sh")
	whole.Dir = root
	whole.Env = append(os.Environ(), "APPROVED_REVISIONS_ENFORCE=1")
	complete, err := whole.CombinedOutput()
	if err != nil {
		t.Fatalf("the signer gate fails on the whole checkout:\n%s", complete)
	}
	if !bytes.Equal(restricted, complete) {
		t.Errorf("the signer gate reads something outside the list:\nwhole checkout:\n%s\nproduction inputs:\n%s", complete, restricted)
	}
}
