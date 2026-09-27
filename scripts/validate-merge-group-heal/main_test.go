package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"

  required-checks:
    runs-on: ubuntu-latest
`

func TestValidateWorkflowContractAcceptsFailClosedHealJob(t *testing.T) {
	t.Parallel()

	if err := validateWorkflowContract(validWorkflow); err != nil {
		t.Fatalf("validateWorkflowContract() error = %v", err)
	}
}

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
      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"`,
			replacement: `          ref: main
      - uses: ./.github/actions/deploy-prod
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
      - uses: ./.github/actions/deploy-prod
        with:
          recover-orphaned-fence: "true"`,
			replacement: `      - uses: actions/checkout@example
        with:
          ref: main
          recover-orphaned-fence: "true"
      - uses: ./.github/actions/deploy-prod`,
			wantError: "heal job is missing orphaned-fence recovery",
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
