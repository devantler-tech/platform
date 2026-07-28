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

// collectInvalidQueues walks every mapping in the document and reports each
// concurrency.queue whose value is outside the enum.
//
// Walking the parsed tree rather than matching lines matters: it accepts quoted
// scalars, ignores a queue key that belongs to anything other than a concurrency
// block, and finds job-level blocks at any nesting depth without knowing the
// workflow's shape.
func collectInvalidQueues(node *yaml.Node) []finding {
	var findings []finding

	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}

		if n.Kind == yaml.MappingNode {
			// Mapping content alternates key, value, key, value.
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				if key.Value == "concurrency" && value.Kind == yaml.MappingNode {
					findings = append(findings, invalidQueueIn(value)...)
				}
			}
		}

		for _, child := range n.Content {
			walk(child)
		}
	}
	walk(node)

	sort.Slice(findings, func(i, j int) bool { return findings[i].line < findings[j].line })

	return findings
}

// invalidQueueIn inspects a single concurrency mapping.
func invalidQueueIn(concurrency *yaml.Node) []finding {
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

// validateFile reports every invalid concurrency.queue in one workflow file.
func validateFile(path string, source []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return fmt.Errorf("%s: could not parse as YAML: %w", path, err)
	}

	findings := collectInvalidQueues(&document)
	if len(findings) == 0 {
		return nil
	}

	var message strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&message,
			"%s:%d: concurrency.queue is %q; GitHub accepts only \"single\" or \"max\".\n",
			path, f.line, f.value)
	}
	message.WriteString(
		"Set each queue above to single (coalesce to the newest pending run) or " +
			"max (queue up to 100 pending runs).")

	return errors.New(message.String())
}

func run(paths []string, stderr io.Writer) error {
	if len(paths) == 0 {
		return errors.New("usage: validate-concurrency-queue <workflow.yaml>...")
	}

	failed := false
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			// Fail closed: an unreadable workflow is not a validated workflow.
			fmt.Fprintf(stderr, "::error::%s: could not read: %v\n", path, err)
			failed = true

			continue
		}
		if err := validateFile(path, source); err != nil {
			fmt.Fprintf(stderr, "::error::%v\n", err)
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
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}
