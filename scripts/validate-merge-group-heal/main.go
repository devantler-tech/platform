// Command validate-merge-group-heal checks that the prod-healing job in a
// workflow can still do its job.
//
// The heal job restores main after a merge-group deploy fails, and it needs
// five things to be true to work at all: it must fire on a failed or cancelled
// merge-group deploy that actually touched k8s and on nothing else; it must
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
	"needs.deploy-prod.result == 'cancelled')"

// The deploy-prod composite recovers an orphaned GHCR fence only when a call
// site opts in — the input is default-off. This exact line is what makes that
// step reachable, so it is pinned rather than left to a reviewer to notice.
const recoveryOptIn = `          recover-orphaned-fence: "true"`

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
			line:        "    needs: [changes, deploy-prod, validate-publication-contract]",
			description: "deploy dependencies",
		},
		{line: "      group: prod-deploy", description: "shared production lock"},
		{line: "      cancel-in-progress: false", description: "non-preempting production lock"},
		{line: "          ref: main", description: "current-main checkout"},
		// Without this, a deploy that dies holding the GHCR synchronization Lease
		// wedges the queue AND this heal job — the exact state the heal exists to
		// clear (#3343). Opting in relaxes no guard: the composite still refuses to
		// release unless the holder is provably terminal and its heartbeat stopped.
		{line: recoveryOptIn, description: "orphaned-fence recovery"},
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
			"heal condition must cover exactly failed and cancelled deploys while excluding success",
		)
	}

	// The heal job is the last line of defence; the deploy job reaching the same
	// composite is what stops the wedge arising at all. Pin both, or a dead deploy
	// still fails the NEXT deploy before any heal gets the chance to clear it.
	deployJob, ok := extractJob(workflow, "deploy-prod")
	if !ok {
		return errors.New("missing deploy-prod job")
	}
	if !containsExactLine(deployJob, recoveryOptIn) {
		return errors.New("deploy job is missing orphaned-fence recovery")
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
