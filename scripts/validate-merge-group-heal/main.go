// Command validate-merge-group-heal checks that the prod-healing job in a
// workflow still runs under exactly the conditions it was designed for.
//
// The heal job restores main after a merge-group deploy fails, so it must fire
// on a failed or cancelled merge-group deploy that actually touched k8s — and
// on nothing else. Widening that guard would let the job run when there is
// nothing to heal; narrowing it would leave main broken after a bad deploy.
// Pinning the condition here means a workflow edit that changes it fails CI
// rather than being discovered during an incident.
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

func validateWorkflowContract(workflow string) error {
	healJob, ok := extractHealJob(workflow)
	if !ok {
		return errors.New("missing heal-prod-on-failure job")
	}

	requirements := []struct {
		line        string
		description string
	}{
		{line: "    needs: [changes, deploy-prod]", description: "deploy dependencies"},
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
			"heal condition must cover exactly failed and cancelled deploys while excluding success",
		)
	}

	return nil
}

func extractHealJob(workflow string) (string, bool) {
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, line := range lines {
		if line == "  heal-prod-on-failure:" {
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
