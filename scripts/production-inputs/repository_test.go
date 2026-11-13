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
	// Nothing but the two pinned gates may run before the restriction.
	if steps[0].Uses != "" || steps[1].Uses != "" || steps[1].ID != "verify_recovery_source" {
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
		if candidate.If != "github.event_name == 'merge_group'" || candidate.Continue != nil ||
			!strings.Contains(candidate.Run, `go run ./scripts/production-inputs changed --base "$base" --head "$head"`) ||
			!strings.Contains(candidate.Run, `base="$(git rev-parse --verify 'HEAD^1^{commit}' 2>/dev/null || true)"`) ||
			!strings.Contains(candidate.Run, `echo 'production=true' >>"$GITHUB_OUTPUT"`) || len(candidate.Env) != 0 {
			return "the classifier step must compare the candidate with its parent and answer true when it fails"
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
		"the classifier fails open": {replace(ci, "            echo 'production=true' >>\"$GITHUB_OUTPUT\"\n", ""), manual, rebuild, deploy},
		"the classifier may be skipped": {replace(ci, "        id: production-inputs\n",
			"        id: production-inputs\n        continue-on-error: true\n"), manual, rebuild, deploy},
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

// A file read out of Git's object store is still there after the working tree
// is restricted, so each such read is reviewed by hand and recorded here with
// what it reads.
func TestReadsFromGitObjectsAreReviewed(t *testing.T) {
	reviewed := map[string]string{
		// Archives the base commit and renders only k8s/providers/hetzner from it.
		"scripts/tests/test-pvc-prune-safety.sh": "archive",
		// Reads the list at two commits and the names of the files that differ.
		"scripts/production-inputs/main.go": "show",
	}
	root := repositoryRoot(t)
	traced, err := trace(root)
	if err != nil {
		t.Fatal(err)
	}
	objectRead := regexp.MustCompile(`\b(archive|show|cat-file -p|cat-file blob|worktree add|ls-tree|restore --source|checkout [^-\n]*--)\b`)
	gitCall := regexp.MustCompile(`\bgit\b|"git"|checkout_git`)
	found := map[string]bool{}
	for _, file := range traced {
		if !followed(file) {
			continue
		}
		for _, line := range strings.Split(code(file, readFile(t, root, file)), "\n") {
			if (gitCall.MatchString(line) || file == "scripts/production-inputs/main.go") && objectRead.MatchString(line) {
				found[file] = true
				if _, ok := reviewed[file]; !ok {
					t.Errorf("%s reads a file from Git's object store; review what it reads and record it here: %s", file, strings.TrimSpace(line))
				}
			}
		}
	}
	for file := range reviewed {
		if !found[file] {
			t.Errorf("%s no longer reads from Git's object store; remove its entry", file)
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
