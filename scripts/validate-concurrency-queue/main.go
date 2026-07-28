// Command validate-concurrency-queue checks that every concurrency.queue value
// in a workflow is one GitHub actually accepts.
//
// actionlint does not know the concurrency.queue key yet, so .github/actionlint.yaml
// silences its "unexpected key" diagnostic for the whole workflow tree. That ignore
// is anchored to one message and leaves every other actionlint check intact, but it
// cannot validate the value: with it in place, queue: maxx lints clean and is
// rejected only by GitHub's own workflow validation, at run time, on the run that
// was supposed to serialize a production deploy.
//
// GitHub accepts exactly single and max. This restores the enum check the ignore
// removes, for both workflow-level and job-level concurrency blocks, so a typo
// fails on the pull request that introduces it instead of during a deploy.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// validQueueValues is the enum GitHub documents for concurrency.queue.
var validQueueValues = map[string]bool{"single": true, "max": true}

// finding is one invalid queue value, located for a fail-with-the-fix message.
type finding struct {
	line  int
	value string
}

// collectInvalidQueues reports each concurrency.queue whose value is outside the
// enum, looking only where the Actions schema actually defines a concurrency
// block: the workflow root, and each job.
//
// Parsing the tree rather than matching lines is what accepts a quoted scalar
// and ignores an inline comment. Restricting the traversal to those two
// positions is what keeps the check from judging free-form data: `concurrency`
// is not a reserved word, so a matrix include entry or a composite action's
// inputs may carry an object of that name whose `queue` property means
// something else entirely. GitHub does not read those as the enum, so neither
// may this validator — flagging one would fail CI on valid configuration.
func collectInvalidQueues(node *yaml.Node) []finding {
	root := mappingOf(node)
	if root == nil {
		return nil
	}

	findings := invalidQueueIn(childMapping(root, "concurrency"))

	// jobs.<job_id>.concurrency — the only job-level position.
	if jobs := childMapping(root, "jobs"); jobs != nil {
		for i := 1; i < len(jobs.Content); i += 2 {
			job := mappingOf(jobs.Content[i])
			findings = append(findings, invalidQueueIn(childMapping(job, "concurrency"))...)
		}
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].line < findings[j].line })

	return findings
}

// mappingOf unwraps a document node to its root mapping, and returns nil for
// anything that is not a mapping (a workflow may be empty, or `concurrency` may
// be a bare group string with no queue at all).
func mappingOf(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}

	return n
}

// childMapping returns the mapping value stored under key, or nil when the key
// is absent or holds a scalar.
func childMapping(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil {
		return nil
	}
	// Mapping content alternates key, value, key, value.
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mappingOf(mapping.Content[i+1])
		}
	}

	return nil
}

// invalidQueueIn inspects a single concurrency mapping. A nil mapping means the
// block is absent or is a bare group string, which carries no queue to check.
func invalidQueueIn(concurrency *yaml.Node) []finding {
	if concurrency == nil {
		return nil
	}

	var findings []finding

	for i := 0; i+1 < len(concurrency.Content); i += 2 {
		key, value := concurrency.Content[i], concurrency.Content[i+1]
		if key.Value != "queue" {
			continue
		}
		// A non-scalar queue (list, mapping) is invalid whatever it contains.
		if value.Kind != yaml.ScalarNode || !validQueueValues[value.Value] {
			findings = append(findings, finding{line: value.Line, value: value.Value})
		}
	}

	return findings
}

// validateFile reports every invalid concurrency.queue in one workflow file. The
// error is reserved for a file that could not be parsed at all; findings are
// returned as data so the caller owns how they are rendered.
func validateFile(path string, source []byte) ([]finding, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return nil, fmt.Errorf("%s: could not parse as YAML: %w", path, err)
	}

	return collectInvalidQueues(&document), nil
}

// formatReport renders findings as a fail-with-the-fix message: where the bad
// value is, what it is, and what to put there instead.
func formatReport(path string, findings []finding) string {
	var message strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&message,
			"%s:%d: concurrency.queue is %q; GitHub accepts only \"single\" or \"max\".\n",
			path, f.line, f.value)
	}
	message.WriteString(
		"Set each queue above to single (coalesce to the newest pending run) or " +
			"max (queue up to 100 pending runs).")

	return message.String()
}

func run(paths []string, stderr io.Writer) error {
	if len(paths) == 0 {
		return errors.New("usage: validate-concurrency-queue <workflow.yaml>")
	}

	failed := false
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			// Fail closed: an unreadable workflow is not a validated workflow.
			_, _ = fmt.Fprintf(stderr, "::error::%s: could not read: %v\n", path, err)
			failed = true

			continue
		}

		findings, err := validateFile(path, source)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "::error::%v\n", err)
			failed = true

			continue
		}
		if len(findings) > 0 {
			_, _ = fmt.Fprintf(stderr, "::error::%s\n", formatReport(path, findings))
			failed = true
		}
	}

	if failed {
		return errors.New("one or more workflows declare an invalid concurrency.queue")
	}

	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}
