package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const validWorkflow = `name: CI

jobs:
  changes:
    runs-on: ubuntu-latest

  deploy-prod:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"

  merge-group-queue-membership:
    needs: [changes, deploy-prod]
    if: github.event_name == 'merge_group' && needs.changes.outputs.k8s == 'true' && needs.deploy-prod.result == 'success'
    permissions:
      pull-requests: read # read the PR's merge-queue state
    outputs:
      evicted: ${{ steps.membership.outputs.evicted }}
    steps:
      - name: checkout
        uses: actions/checkout@example
      - name: read
        id: membership
        env:
          EVICTED_HEAD_REF: ${{ github.event.merge_group.head_ref }}
          EVICTED_GROUP_CREATED_AT: ${{ github.event.merge_group.head_commit.timestamp }}
        run: scripts/merge-group-evicted.sh

  heal-prod-on-failure:
    needs: [changes, deploy-prod, validate-publication-contract, merge-group-queue-membership]
    concurrency:
      group: prod-deploy
      cancel-in-progress: false
    if: >-
      always() &&
      github.event_name == 'merge_group' &&
      needs.changes.outputs.k8s == 'true' &&
      (needs.deploy-prod.result == 'failure' ||
       needs.deploy-prod.result == 'cancelled' ||
       (needs.deploy-prod.result == 'success' &&
        needs.merge-group-queue-membership.outputs.evicted == 'true'))
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@example
        with:
          ref: main
` + recoveryBaselineStep + `      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"
` + orphanCheckStep + `
  required-checks:
    runs-on: ubuntu-latest
`

// recoveryBaselineStep records the checked-out commit before the recovery deploy.
const recoveryBaselineStep = `      - name: record the recovery checkout
        id: recovery-baseline
        shell: bash
        run: |
          recovery_sha="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
            git --no-replace-objects -C "${GITHUB_WORKSPACE:?the recovery checkout workspace is required}" \
            rev-parse --verify 'HEAD^{commit}')"
          [[ "${recovery_sha}" =~ ^[0-9a-f]{40}$ ]]
          printf 'sha=%s\n' "${recovery_sha}" >>"${GITHUB_OUTPUT}"

`

// orphanCheckStep supplies the merge-group boundary and the actual recovery checkout.
const orphanCheckStep = `      - name: verify nothing outlived the failed deploy
        shell: bash
        env:
          FLUX_ORPHANS_SINCE: ${{ github.event.merge_group.head_commit.timestamp }}
          FLUX_ORPHANS_BASE_SHA: ${{ github.event.merge_group.base_sha }}
          FLUX_ORPHANS_RECOVERY_SHA: ${{ steps.recovery-baseline.outputs.sha }}
        run: |
          if [[ ! -f scripts/check-flux-orphaned-objects.sh ]]; then
            echo '- Flux orphaned objects: main predates the check; not run.' >>"${GITHUB_STEP_SUMMARY}"
            exit 0
          fi
          : "${FLUX_ORPHANS_SINCE:?the merge group creation time is required}"
          ./scripts/check-flux-orphaned-objects.sh
`

// TestValidateWorkflowContractAcceptsFailClosedHealJob keeps the complete recovery path valid.
func TestValidateWorkflowContractAcceptsFailClosedHealJob(t *testing.T) {
	t.Parallel()

	if err := validateWorkflowContract(validWorkflow); err != nil {
		t.Fatalf("validateWorkflowContract() error = %v", err)
	}
}

// TestRecoveryBaselineContract rejects missing, aliased and unproduced residue inputs.
func TestRecoveryBaselineContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, old, replacement, want string
	}{
		{"missing base", "          FLUX_ORPHANS_BASE_SHA: ${{ github.event.merge_group.base_sha }}\n", "", "merge-group base"},
		{"missing recovery", "          FLUX_ORPHANS_RECOVERY_SHA: ${{ steps.recovery-baseline.outputs.sha }}\n", "", "recovery checkout input"},
		{"recovery aliases base", "${{ steps.recovery-baseline.outputs.sha }}", "${{ github.event.merge_group.base_sha }}", "recovery checkout input"},
		{"base aliases recovery", "FLUX_ORPHANS_BASE_SHA: ${{ github.event.merge_group.base_sha }}", "FLUX_ORPHANS_BASE_SHA: ${{ steps.recovery-baseline.outputs.sha }}", "merge-group base"},
		{"missing producing step", recoveryBaselineStep, "", "recorded recovery checkout step"},
		{"records wrong commit", "rev-parse --verify 'HEAD^{commit}'", "rev-parse --verify 'origin/main^{commit}'", "recorded recovery checkout step"},
		{"drops output", "          printf 'sha=%s\\n' \"${recovery_sha}\" >>\"${GITHUB_OUTPUT}\"\n", "", "recorded recovery checkout step"},
		{"conditional capture", "        id: recovery-baseline\n", "        id: recovery-baseline\n        if: ${{ false }}\n", "recorded recovery checkout step"},
		{"wrong capture shell", "        id: recovery-baseline\n        shell: bash\n", "        id: recovery-baseline\n        shell: sh\n", "recorded recovery checkout step"},
		{"capture directory override", "        id: recovery-baseline\n", "        id: recovery-baseline\n        working-directory: ${{ runner.temp }}/candidate-checkout\n", "recorded recovery checkout step"},
		{"capture environment override", "        id: recovery-baseline\n", "        id: recovery-baseline\n        env:\n          GIT_DIR: ${{ runner.temp }}/candidate.git\n", "recorded recovery checkout step"},
		{"unvalidated commit", "          [[ \"${recovery_sha}\" =~ ^[0-9a-f]{40}$ ]]\n", "", "recorded recovery checkout step"},
		{"capture before checkout", "          ref: main\n" + recoveryBaselineStep, recoveryBaselineStep + "          ref: main\n", "recorded recovery checkout step"},
		{"capture after deploy", recoveryBaselineStep + "      - uses: ./.github/actions/deploy-prod\n        with:\n          recover-orphaned-fence: \"true\"\n", "      - uses: ./.github/actions/deploy-prod\n        with:\n          recover-orphaned-fence: \"true\"\n" + recoveryBaselineStep, "must follow checkout and precede deployment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workflow := strings.Replace(validWorkflow, tt.old, tt.replacement, 1)
			if workflow == validWorkflow {
				t.Fatal("fixture mutation did not change the workflow")
			}
			err := validateWorkflowContract(workflow)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateWorkflowContract() = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestActualRecoveryCaptureUsesWorkspace executes the workflow's capture, not a
// copied script, so changing its Git context cannot record another repository.
func TestActualRecoveryCaptureUsesWorkspace(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID  string `yaml:"id"`
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["heal-prod-on-failure"].Steps {
		if step.ID == "recovery-baseline" {
			if script != "" {
				t.Fatal("multiple recovery capture steps")
			}
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("actual recovery capture script is missing")
	}

	// Ignore the host's Git configuration and environment. These repositories
	// contain only disposable unsigned fixture commits and never have a remote.
	var fixtureEnv []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "GITHUB_WORKSPACE=") && !strings.HasPrefix(value, "GITHUB_OUTPUT=") {
			fixtureEnv = append(fixtureEnv, value)
		}
	}
	fixtureEnv = append(fixtureEnv, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	makeRepository := func(name string) (string, string) {
		dir := filepath.Join(t.TempDir(), name)
		git := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Env = fixtureEnv
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("fixture git %v: %v: %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		git("init", "--quiet", "--initial-branch=main", "--object-format=sha1", dir)
		git("-C", dir, "-c", "user.name=Recovery Capture Fixture", "-c", "user.email=recovery-capture-fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", name)
		return dir, git("-C", dir, "rev-parse", "HEAD")
	}
	workspace, workspaceSHA := makeRepository("workspace")
	alternate, alternateSHA := makeRepository("alternate")
	if workspaceSHA == alternateSHA {
		t.Fatal("fixture repositories must have different commits")
	}

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"different cwd", nil},
		{"inherited Git directories", []string{"GIT_DIR=" + filepath.Join(alternate, ".git"), "GIT_WORK_TREE=" + alternate, "GIT_COMMON_DIR=" + filepath.Join(alternate, ".git")}},
		{"inherited common directory", []string{"GIT_COMMON_DIR=" + filepath.Join(alternate, ".git")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "capture-output")
			cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = alternate
			cmd.Env = append(append([]string{}, fixtureEnv...), "GITHUB_WORKSPACE="+workspace, "GITHUB_OUTPUT="+output)
			cmd.Env = append(cmd.Env, tc.env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("actual recovery capture failed: %v: %s", err, out)
			}
			got, err := os.ReadFile(output)
			if err != nil || string(got) != "sha="+workspaceSHA+"\n" {
				t.Fatalf("capture must emit workspace commit %s, not alternate %s: got %q, error %v", workspaceSHA, alternateSHA, got, err)
			}
		})
	}
}

// TestValidateWorkflowContractRejectsBrokenHealContracts removes one recovery safeguard per case.
func TestValidateWorkflowContractRejectsBrokenHealContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		old         string
		replacement string
		wantError   string
	}{
		{
			name:        "missing heal job",
			old:         "  heal-prod-on-failure:",
			replacement: "  heal-prod-disabled:",
			wantError:   "missing heal-prod-on-failure job",
		},
		{
			name:        "missing deploy dependencies",
			old:         "    needs: [changes, deploy-prod, validate-publication-contract, merge-group-queue-membership]",
			replacement: "    needs: [changes]",
			wantError:   "missing deploy dependencies",
		},
		{
			name:        "missing production lock",
			old:         "      group: prod-deploy",
			replacement: "      group: other-deploy",
			wantError:   "missing shared production lock",
		},
		{
			name:        "preempting production lock",
			old:         "      cancel-in-progress: false",
			replacement: "      cancel-in-progress: true",
			wantError:   "missing non-preempting production lock",
		},
		{
			name:        "missing main checkout",
			old:         "          ref: main",
			replacement: "          ref: merge-group",
			wantError:   "missing current-main checkout",
		},
		{
			name:        "implicit condition",
			old:         "    if: >-",
			replacement: "    if: |",
			wantError:   "must use an explicit multiline condition",
		},
		{
			name:        "condition heals every successful deploy",
			old:         "needs.deploy-prod.result == 'failure'",
			replacement: "needs.deploy-prod.result == 'success'",
			wantError:   "must cover exactly failed, cancelled, and evicted-after-success deploys",
		},
		{
			// Without the evicted branch a successful deploy whose PR already
			// left the queue keeps its unmerged artifact in prod (#3091).
			name:        "condition drops the evicted branch",
			old:         "needs.merge-group-queue-membership.outputs.evicted == 'true'",
			replacement: "needs.merge-group-queue-membership.outputs.evicted == 'false'",
			wantError:   "must cover exactly failed, cancelled, and evicted-after-success deploys",
		},
		{
			name:        "missing queue-membership job",
			old:         "  merge-group-queue-membership:",
			replacement: "  merge-group-queue-check:",
			wantError:   "missing merge-group-queue-membership job",
		},
		{
			name:        "queue-membership job runs after an unsuccessful deploy",
			old:         "needs.deploy-prod.result == 'success'\n",
			replacement: "needs.deploy-prod.result == 'failure'\n",
			wantError:   "queue-membership job is missing successful-deploy condition",
		},
		{
			name:        "queue-membership job does not export its answer",
			old:         "      evicted: ${{ steps.membership.outputs.evicted }}",
			replacement: "      evicted: ${{ steps.other.outputs.evicted }}",
			wantError:   "queue-membership job is missing evicted output",
		},
		{
			name:        "queue-membership job reads the wrong ref",
			old:         "          EVICTED_HEAD_REF: ${{ github.event.merge_group.head_ref }}",
			replacement: "          EVICTED_HEAD_REF: ${{ github.ref }}",
			wantError:   "membership step is missing merge-group head ref input",
		},
		{
			// Without the group's creation time a re-enqueued PR's replacement entry
			// cannot be told apart from this group's own entry.
			name:        "queue-membership job reads the wrong creation time",
			old:         "          EVICTED_GROUP_CREATED_AT: ${{ github.event.merge_group.head_commit.timestamp }}",
			replacement: "          EVICTED_GROUP_CREATED_AT: ${{ github.event.repository.pushed_at }}",
			wantError:   "membership step is missing merge-group creation time input",
		},
		{
			// The output reads steps.membership, so the id on another step leaves
			// the check's answer unexported even though every line still exists.
			name:        "membership id moved onto another step",
			old:         "      - name: checkout\n        uses: actions/checkout@example\n      - name: read\n        id: membership\n",
			replacement: "      - name: checkout\n        id: membership\n        uses: actions/checkout@example\n      - name: read\n",
			wantError:   "membership step is missing merge-group head ref input",
		},
		{
			name:        "queue-membership job suppresses failure",
			old:         "    permissions:\n      pull-requests: read",
			replacement: "    continue-on-error: true\n    permissions:\n      pull-requests: read",
			wantError:   "must not suppress a failed check with continue-on-error",
		},
		{
			name:        "membership step suppresses failure",
			old:         "        run: scripts/merge-group-evicted.sh",
			replacement: "        continue-on-error: true\n        run: scripts/merge-group-evicted.sh",
			wantError:   "must not suppress a failed check with continue-on-error",
		},
		{
			name:        "membership step can be skipped",
			old:         "        run: scripts/merge-group-evicted.sh",
			replacement: "        if: ${{ false }}\n        run: scripts/merge-group-evicted.sh",
			wantError:   "membership step must not carry a condition that can skip it",
		},
		{
			name:        "queue-membership job cannot read pull requests",
			old:         "      pull-requests: read # read the PR's merge-queue state",
			replacement: "      pull-requests: none",
			wantError:   "queue-membership job is missing pull-request read permission",
		},
		{
			name:        "condition drops cancellation",
			old:         "needs.deploy-prod.result == 'cancelled'",
			replacement: "needs.deploy-prod.result == 'failure'",
			wantError:   "must cover exactly failed, cancelled, and evicted-after-success deploys",
		},
		{
			// The opt-in is what makes the composite's recovery step reachable at
			// all, so dropping it silently restores the wedge this pin exists to
			// prevent -- the heal cannot clear a Lease a dead deploy left held.
			name: "heal job drops orphaned-fence recovery",
			old: `          ref: main
` + recoveryBaselineStep + `      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"`,
			replacement: `          ref: main
` + recoveryBaselineStep + `      - uses: ./.github/actions/deploy-prod
        with:
          sops-age-key: placeholder`,
			wantError: "heal job is missing orphaned-fence recovery",
		},
		{
			// A job that no longer calls the composite at all is a different break
			// from one that calls it without the opt-in, so they get separate cases.
			name: "deploy job does not reach the deploy composite",
			old: `  deploy-prod:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"`,
			replacement: "  deploy-prod:\n    runs-on: ubuntu-latest",
			wantError:   "deploy job does not reach the shared deploy composite",
		},
		{
			name: "deploy job drops orphaned-fence recovery",
			old: `  deploy-prod:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"`,
			replacement: `  deploy-prod:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/deploy-prod
        with:
          sops-age-key: placeholder`,
			wantError: "deploy job is missing orphaned-fence recovery",
		},
		{
			name:        "missing deploy job",
			old:         "  deploy-prod:",
			replacement: "  deploy-prod-disabled:",
			wantError:   "missing deploy-prod job",
		},
		{
			// The opt-in must be bound to the step that reaches the composite.
			// Relocating it to a neighbouring step leaves every line the validator
			// used to look for present in the job, while the composite itself still
			// runs on its default "false" -- the exact state this pin exists to catch.
			name: "heal job relocates the opt-in off the deploy step",
			old: `      - uses: actions/checkout@example
        with:
          ref: main
` + recoveryBaselineStep + `      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"`,
			replacement: `      - uses: actions/checkout@example
        with:
          ref: main
          recover-orphaned-fence: "true"
` + recoveryBaselineStep + `      - uses: ./.github/actions/deploy-prod`,
			wantError: "heal job is missing orphaned-fence recovery",
		},
		{
			// Re-deploying main cannot remove what a failed revision applied with
			// prune disabled, so without the check a green heal says nothing about
			// whether prod matches main (#3502).
			name:        "heal job drops the orphaned-object check",
			old:         "          ./scripts/check-flux-orphaned-objects.sh\n",
			replacement: "          echo healed\n",
			wantError:   "heal job does not check for objects the failed deploy left outside every Flux inventory",
		},
		{
			// Before the re-deploy, the check would read the failed revision's
			// state rather than the restored one.
			name: "orphaned-object check runs before the re-deploy",
			old: `      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"
` + orphanCheckStep,
			replacement: orphanCheckStep + `      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"
`,
			wantError: "orphaned-object check must run after the heal re-deploys main",
		},
		{
			// Any early exit lets the heal pass without the check having run.
			name:        "orphaned-object check exits before it runs",
			old:         "          : \"${FLUX_ORPHANS_SINCE:?the merge group creation time is required}\"\n",
			replacement: "          exit 0\n",
			wantError:   "orphaned-object check must run exactly the pinned script",
		},
		{
			// Without a boundary the check fails on every older orphan, which a
			// retirement leaves on purpose.
			name:        "orphaned-object check tolerates a missing boundary",
			old:         "          : \"${FLUX_ORPHANS_SINCE:?the merge group creation time is required}\"\n",
			replacement: "",
			wantError:   "orphaned-object check must run exactly the pinned script",
		},
		{
			// A later boundary would pass the failed deploy's own residue off as
			// an older orphan, which the check only warns about.
			name:        "orphaned-object check judges residue from the wrong time",
			old:         "          FLUX_ORPHANS_SINCE: ${{ github.event.merge_group.head_commit.timestamp }}",
			replacement: "          FLUX_ORPHANS_SINCE: ${{ github.event.repository.pushed_at }}",
			wantError:   "orphaned-object check is missing the merge-group creation time",
		},
		{
			name:        "orphaned-object check can be skipped",
			old:         "      - name: verify nothing outlived the failed deploy\n        shell: bash\n",
			replacement: "      - name: verify nothing outlived the failed deploy\n        if: ${{ false }}\n        shell: bash\n",
			wantError:   "orphaned-object check must not carry a condition that can skip it",
		},
		{
			name:        "orphaned-object check suppresses failure",
			old:         "      - name: verify nothing outlived the failed deploy\n        shell: bash\n",
			replacement: "      - name: verify nothing outlived the failed deploy\n        continue-on-error: true\n        shell: bash\n",
			wantError:   "heal job must not suppress a failed check with continue-on-error",
		},
		{
			name:        "heal job suppresses failure",
			old:         "    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@example\n",
			replacement: "    runs-on: ubuntu-latest\n    continue-on-error: true\n    steps:\n      - uses: actions/checkout@example\n",
			wantError:   "heal job must not suppress a failed check with continue-on-error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow := strings.Replace(validWorkflow, tt.old, tt.replacement, 1)
			if workflow == validWorkflow {
				t.Fatalf("test replacement %q did not change workflow", tt.old)
			}

			err := validateWorkflowContract(workflow)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("validateWorkflowContract() error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

// TestRunReportsValidationResult checks success and failure diagnostics at the command boundary.
func TestRunReportsValidationResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		workflow   string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "valid contract",
			workflow:   validWorkflow,
			wantCode:   0,
			wantStdout: "Merge-group heal workflow contract passed.\n",
		},
		{
			name:       "invalid contract",
			workflow:   strings.Replace(validWorkflow, "          ref: main", "          ref: merge-group", 1),
			wantCode:   1,
			wantStderr: "merge-group heal contract: heal job is missing current-main checkout\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflowPath := filepath.Join(t.TempDir(), "ci.yaml")
			if err := os.WriteFile(workflowPath, []byte(tt.workflow), 0o600); err != nil {
				t.Fatalf("write workflow: %v", err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if got := run(workflowPath, &stdout, &stderr); got != tt.wantCode {
				t.Fatalf("run() code = %d, want %d", got, tt.wantCode)
			}
			if got := stdout.String(); got != tt.wantStdout {
				t.Fatalf("run() stdout = %q, want %q", got, tt.wantStdout)
			}
			if got := stderr.String(); got != tt.wantStderr {
				t.Fatalf("run() stderr = %q, want %q", got, tt.wantStderr)
			}
		})
	}
}

// TestRunReportsWorkflowReadFailure prevents a missing workflow from reporting success.
func TestRunReportsWorkflowReadFailure(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	workflowPath := filepath.Join(t.TempDir(), "missing.yaml")

	if got := run(workflowPath, &stdout, &stderr); got != 1 {
		t.Fatalf("run() code = %d, want 1", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("run() stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "merge-group heal contract: read workflow:") {
		t.Fatalf("run() stderr = %q, want read failure", got)
	}
}

// TestRunCLIUsesExplicitWorkflowPathOutsideRepository rejects reliance on the current directory.
func TestRunCLIUsesExplicitWorkflowPathOutsideRepository(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "ci.yaml")
	if err := os.WriteFile(workflowPath, []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Chdir(t.TempDir())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if got := runCLI([]string{workflowPath}, &stdout, &stderr); got != 0 {
		t.Fatalf("runCLI() code = %d, want 0; stderr = %q", got, stderr.String())
	}
	if got := stdout.String(); got != "Merge-group heal workflow contract passed.\n" {
		t.Fatalf("runCLI() stdout = %q", got)
	}
}

// TestRunCLIRequiresOneWorkflowPath rejects absent and ambiguous workflow arguments.
func TestRunCLIRequiresOneWorkflowPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "missing path"},
		{name: "extra path", args: []string{"ci.yaml", "other.yaml"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if got := runCLI(tt.args, &stdout, &stderr); got != 2 {
				t.Fatalf("runCLI() code = %d, want 2", got)
			}
			if stdout.Len() != 0 {
				t.Fatalf("runCLI() stdout = %q, want empty", stdout.String())
			}
			if got := stderr.String(); got != "usage: validate-merge-group-heal <workflow-path>\n" {
				t.Fatalf("runCLI() stderr = %q", got)
			}
		})
	}
}
