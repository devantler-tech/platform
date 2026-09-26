// Command validate-merge-group-heal checks that the prod-healing job in a
// workflow can still do its job.
//
// The heal job restores main after a merge-group deploy fails, and it needs
// five things to be true to work at all: it must fire on a failed or cancelled
// merge-group deploy that actually touched k8s, or on a successful one whose
// PR had already left the queue (#3091), and on nothing else; it must
// hold the shared prod-deploy lock; that lock must not be preemptible, or the
// heal is cancelled by the next deploy midway through; it must check out
// main, or it restores the wrong revision; and it must opt in to orphaned-fence
// recovery, or it cannot clear the GHCR Lease a dead deploy left held.
//
// Each is one line of workflow YAML, and none of them fails loudly when it is
// wrong — the damage shows up later, during an incident, when the heal either
// does not run or restores the wrong thing. Pinning all five here turns that
// into a CI failure on the pull request that breaks one.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const expectedHealCondition = "always() && " +
	"github.event_name == 'merge_group' && " +
	"needs.changes.outputs.k8s == 'true' && " +
	"(needs.deploy-prod.result == 'failure' || " +
	"needs.deploy-prod.result == 'cancelled' || " +
	"(needs.deploy-prod.result == 'success' && " +
	"needs.merge-group-queue-membership.outputs.evicted == 'true'))"

// The deploy-prod composite recovers an orphaned GHCR fence only when a call
// site opts in — the input is default-off. This exact line is what makes that
// step reachable, so it is pinned rather than left to a reviewer to notice.
const recoveryOptIn = `          recover-orphaned-fence: "true"`

// The shared composite both prod-deploy paths call.
const deployCompositePath = "./.github/actions/deploy-prod"

func validateWorkflowContract(workflow string) error {
	healJob, ok := extractJob(workflow, "heal-prod-on-failure")
	if !ok {
		return errors.New("missing heal-prod-on-failure job")
	}

	requirements := []struct {
		line        string
		description string
	}{
		// validate-publication-contract is required by the DR signing contract:
		// every ci.yaml job that reaches production must WAIT for the publication
		// gate, and this job re-deploys main's artifact. Pinned as an exact line
		// here so the two contracts cannot drift into asserting different
		// dependency sets over the same job.
		{
			line:        "    needs: [changes, deploy-prod, validate-publication-contract, merge-group-queue-membership]",
			description: "deploy dependencies",
		},
		{line: "      group: prod-deploy", description: "shared production lock"},
		{line: "      cancel-in-progress: false", description: "non-preempting production lock"},
		{line: "          ref: main", description: "current-main checkout"},
	}
	for _, requirement := range requirements {
		if !containsExactLine(healJob, requirement.line) {
			return fmt.Errorf("heal job is missing %s", requirement.description)
		}
	}

	condition, ok := extractMultilineCondition(healJob)
	if !ok {
		return errors.New("heal job must use an explicit multiline condition")
	}
	if strings.Join(strings.Fields(condition), " ") != expectedHealCondition {
		return errors.New(
			"heal condition must cover exactly failed, cancelled, and evicted-after-success deploys",
		)
	}

	if err := validateMembershipJob(workflow); err != nil {
		return err
	}

	// Both jobs reach the same composite, and the opt-in is checked against the
	// STEP rather than the job: a line accepted anywhere in the job would still
	// pass after an edit moved it onto an unrelated step, leaving the composite
	// on its default "false". The validator would then vouch for precisely the
	// state it exists to catch. The heal job is the last line of defence; the
	// deploy job reaching the composite is what stops the wedge arising at all.
	for _, target := range []struct{ key, label string }{
		{"deploy-prod", "deploy"},
		{"heal-prod-on-failure", "heal"},
	} {
		job, ok := extractJob(workflow, target.key)
		if !ok {
			return fmt.Errorf("missing %s job", target.key)
		}
		step, ok := extractDeployStep(job)
		if !ok {
			return fmt.Errorf("%s job does not reach the shared deploy composite", target.label)
		}
		if !containsExactLine(step, recoveryOptIn) {
			return fmt.Errorf("%s job is missing orphaned-fence recovery", target.label)
		}
	}

	return nil
}

// validateMembershipJob pins the job that tells the heal whether a successful
// deploy's PR already left the merge queue (#3091). If it stops running after a
// successful deploy, stops reading the merge group's own ref, stops
// exporting its answer, or is allowed to fail quietly, the heal's evicted
// branch reads an empty output and silently never fires — the same unmerged
// artifact left in prod that the heal exists to remove.
//
// It must also run after a failed or cancelled deploy. There its answer is not
// read; its wait for the queue to drain is what holds the heal back until every
// later merge group has either merged or left. Without that wait the heal can
// overwrite a later group's speculative deploy with an older main, and that
// group then merges with nothing re-deploying it (#2838).
func validateMembershipJob(workflow string) error {
	job, ok := extractJob(workflow, "merge-group-queue-membership")
	if !ok {
		return errors.New("missing merge-group-queue-membership job")
	}
	jobRequirements := []struct {
		line        string
		description string
	}{
		{
			line:        "    needs: [changes, deploy-prod]",
			description: "deploy dependency",
		},
		{
			line: "    if: always() && github.event_name == 'merge_group' && " +
				"needs.changes.outputs.k8s == 'true' && " +
				"(needs.deploy-prod.result == 'success' || " +
				"needs.deploy-prod.result == 'failure' || " +
				"needs.deploy-prod.result == 'cancelled')",
			description: "every-deploy-outcome condition",
		},
		{line: "      pull-requests: read # read the PR's merge-queue state", description: "pull-request read permission"},
		{line: "      evicted: ${{ steps.membership.outputs.evicted }}", description: "evicted output"},
	}
	for _, requirement := range jobRequirements {
		if !containsExactLine(job, requirement.line) {
			return fmt.Errorf("queue-membership job is missing %s", requirement.description)
		}
	}

	// A failed read must fail the job. continue-on-error at either scope lets it
	// succeed with no output, which the heal reads as "not evicted".
	for _, line := range strings.Split(job, "\n") {
		if strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(line), "- "), "continue-on-error:") {
			return errors.New("queue-membership job must not suppress a failed check with continue-on-error")
		}
	}

	// The output names the step by id, so the id, its inputs and the command
	// must belong to ONE step; lines matched anywhere in the job would still
	// pass after the id moved onto an unrelated step.
	step, ok := extractStep(job, func(line string) bool {
		return strings.TrimPrefix(strings.TrimSpace(line), "- ") == "id: membership"
	})
	if !ok {
		return errors.New("queue-membership job is missing membership step id")
	}
	// A skipped step succeeds with no output, which the heal also reads as
	// "not evicted", so the step runs whenever its job does.
	for _, line := range strings.Split(step, "\n") {
		if strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(line), "- "), "if:") {
			return errors.New("membership step must not carry a condition that can skip it")
		}
	}
	stepRequirements := []struct {
		line        string
		description string
	}{
		{
			line:        "          EVICTED_HEAD_REF: ${{ github.event.merge_group.head_ref }}",
			description: "merge-group head ref input",
		},
		{
			line:        "          EVICTED_GROUP_CREATED_AT: ${{ github.event.merge_group.head_commit.timestamp }}",
			description: "merge-group creation time input",
		},
		{line: "        run: scripts/merge-group-evicted.sh", description: "eviction check"},
	}
	for _, requirement := range stepRequirements {
		if !containsExactLine(step, requirement.line) {
			return fmt.Errorf("membership step is missing %s", requirement.description)
		}
	}
	return nil
}

func extractJob(workflow string, jobKey string) (string, bool) {
	lines := strings.Split(workflow, "\n")
	start := -1
	header := "  " + jobKey + ":"
	for i, line := range lines {
		if line == header {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}

	end := len(lines)
	for i := start; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "  ") &&
			!strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(line, ":") {
			end = i
			break
		}
	}

	return strings.Join(lines[start:end], "\n"), true
}

// extractDeployStep returns the one step in a job that reaches the shared
// deploy composite, so an input can be checked against THAT step rather than
// against the whole job. It walks back from the `uses:` line to the list-item
// start, because a step may declare `with:` before `uses:`.
func extractDeployStep(job string) (string, bool) {
	// A step may write `- uses: …` on the list-item line itself or put `uses:`
	// on its own line under `- name:`; both spellings are the same step.
	return extractStep(job, func(line string) bool {
		return strings.TrimPrefix(strings.TrimSpace(line), "- ") == "uses: "+deployCompositePath
	})
}

// extractStep returns the step containing the first line that matches, from
// its list-item start to the next sibling step.
func extractStep(job string, matches func(string) bool) (string, bool) {
	lines := strings.Split(job, "\n")
	target := -1
	for i, line := range lines {
		if matches(line) {
			target = i
			break
		}
	}
	if target < 0 {
		return "", false
	}

	start := 0
	for i := target; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(lines[i], " "), "- ") {
			start = i
			break
		}
	}
	indent := len(lines[start]) - len(strings.TrimLeft(lines[start], " "))

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimLeft(lines[i], " ")
		if trimmed == "" {
			continue
		}
		lineIndent := len(lines[i]) - len(trimmed)
		if lineIndent < indent || (lineIndent == indent && strings.HasPrefix(trimmed, "- ")) {
			end = i
			break
		}
	}

	return strings.Join(lines[start:end], "\n"), true
}

func containsExactLine(block string, want string) bool {
	for _, line := range strings.Split(block, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func extractMultilineCondition(job string) (string, bool) {
	lines := strings.Split(job, "\n")
	for i, line := range lines {
		if line != "    if: >-" {
			continue
		}

		conditionLines := make([]string, 0, 5)
		for _, conditionLine := range lines[i+1:] {
			if !strings.HasPrefix(conditionLine, "      ") {
				break
			}
			conditionLines = append(conditionLines, conditionLine)
		}
		if len(conditionLines) == 0 {
			return "", false
		}
		return strings.Join(conditionLines, "\n"), true
	}

	return "", false
}

func run(workflowPath string, stdout io.Writer, stderr io.Writer) int {
	workflow, err := os.ReadFile(workflowPath) //nolint:gosec // The explicit CLI path is the validator input.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "merge-group heal contract: read workflow: %v\n", err)
		return 1
	}

	if err := validateWorkflowContract(string(workflow)); err != nil {
		_, _ = fmt.Fprintf(stderr, "merge-group heal contract: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "Merge-group heal workflow contract passed.")
	return 0
}

func runCLI(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: validate-merge-group-heal <workflow-path>")
		return 2
	}
	return run(args[0], stdout, stderr)
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
