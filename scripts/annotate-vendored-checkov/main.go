// Command annotate-vendored-checkov adds the repository's reviewed Checkov
// dispositions to pinned upstream operator bundles without reformatting them.
//
// The upstream files are deliberately kept byte-for-byte apart from these
// annotations. The updater can therefore replace a bundle, run this command,
// and fail closed when a target resource is renamed or removed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	clusterRoleReason = "Vendored upstream operator RBAC; changes must go through the pinned vendor update path tracked by platform issue 2899."
	deploymentReason  = "Pinned upstream operator deployment; changes must go through the vendor update path tracked by platform issue 2899."
)

type targetSpec struct {
	kind   string
	name   string
	checks []string
	reason string
}

type manifestIdentity struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
}

var bundleTargets = map[string][]targetSpec{
	"cdi": {
		{kind: "ClusterRole", name: "cdi-operator-cluster", checks: []string{"CKV_K8S_155"}, reason: clusterRoleReason},
		{kind: "Deployment", name: "cdi-operator", checks: deploymentChecks(), reason: deploymentReason},
	},
	"kubevirt": {
		{kind: "ClusterRole", name: "kubevirt-operator", checks: []string{"CKV_K8S_155"}, reason: clusterRoleReason},
		{kind: "Deployment", name: "virt-operator", checks: deploymentChecks(), reason: deploymentReason},
	},
}

func deploymentChecks() []string {
	return []string{
		"CKV_K8S_11",
		"CKV_K8S_13",
		"CKV_K8S_15",
		"CKV_K8S_22",
		"CKV_K8S_38",
		"CKV_K8S_40",
		"CKV_K8S_43",
	}
}

func main() {
	bundle := flag.String("bundle", "", "bundle to annotate: cdi or kubevirt")
	flag.Parse()
	if flag.NArg() != 0 {
		fail("unexpected positional arguments")
	}

	targets, ok := bundleTargets[*bundle]
	if !ok {
		fail("unsupported bundle %q", *bundle)
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail("read bundle: %v", err)
	}
	output, err := annotateBundle(string(input), targets)
	if err != nil {
		fail("annotate %s bundle: %v", *bundle, err)
	}
	if _, err := io.WriteString(os.Stdout, output); err != nil {
		fail("write bundle: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "annotate-vendored-checkov: "+format+"\n", args...)
	os.Exit(1)
}

func annotateBundle(input string, targets []targetSpec) (string, error) {
	if input == "" {
		return "", errors.New("bundle is empty")
	}

	hasTrailingNewline := strings.HasSuffix(input, "\n")
	trimmed := strings.TrimSuffix(input, "\n")
	lines := strings.Split(trimmed, "\n")
	documents := splitDocuments(lines)
	found := make(map[string]int, len(targets))
	output := make([]string, 0, len(lines)+len(targets)*10)

	for _, document := range documents {
		identity, err := readIdentity(document)
		if err != nil {
			return "", err
		}

		annotated := document
		for _, target := range targets {
			if identity.Kind != target.kind || identity.Metadata.Name != target.name {
				continue
			}
			key := target.kind + "/" + target.name
			found[key]++
			annotated, err = annotateDocument(annotated, target)
			if err != nil {
				return "", fmt.Errorf("%s: %w", key, err)
			}
		}
		output = append(output, annotated...)
	}

	for _, target := range targets {
		key := target.kind + "/" + target.name
		if found[key] != 1 {
			return "", fmt.Errorf("expected exactly one %s, found %d", key, found[key])
		}
	}

	result := strings.Join(output, "\n")
	if hasTrailingNewline {
		result += "\n"
	}
	return result, nil
}

func splitDocuments(lines []string) [][]string {
	starts := []int{0}
	for index, line := range lines {
		if index > 0 && line == "---" {
			starts = append(starts, index)
		}
	}

	documents := make([][]string, 0, len(starts))
	for index, start := range starts {
		end := len(lines)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		documents = append(documents, append([]string(nil), lines[start:end]...))
	}
	return documents
}

func readIdentity(lines []string) (manifestIdentity, error) {
	var identity manifestIdentity
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &identity); err != nil {
		return identity, fmt.Errorf("parse vendor document: %w", err)
	}
	return identity, nil
}

func annotateDocument(lines []string, target targetSpec) ([]string, error) {
	metadataIndex := -1
	metadataEnd := len(lines)
	for index, line := range lines {
		if line == "metadata:" {
			if metadataIndex != -1 {
				return nil, errors.New("multiple top-level metadata mappings")
			}
			metadataIndex = index
			continue
		}
		if metadataIndex != -1 && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			metadataEnd = index
			break
		}
	}
	if metadataIndex == -1 {
		return nil, errors.New("top-level metadata mapping is missing")
	}

	annotationsIndex := -1
	annotationsEnd := metadataEnd
	for index := metadataIndex + 1; index < metadataEnd; index++ {
		if lines[index] == "  annotations:" {
			if annotationsIndex != -1 {
				return nil, errors.New("multiple metadata.annotations mappings")
			}
			annotationsIndex = index
			continue
		}
		if annotationsIndex != -1 && lines[index] != "" && !strings.HasPrefix(lines[index], "    ") && !strings.HasPrefix(strings.TrimSpace(lines[index]), "#") {
			annotationsEnd = index
			break
		}
	}

	var insertAt int
	if annotationsIndex == -1 {
		annotationsIndex = metadataIndex + 1
		insertAt = annotationsIndex + 1
		lines = insertLines(lines, annotationsIndex, []string{"  annotations:"})
	} else {
		insertAt = annotationsEnd
	}

	existing, highest, err := existingCheckovAnnotations(lines, annotationsIndex+1, insertAt)
	if err != nil {
		return nil, err
	}
	additions := make([]string, 0, len(target.checks))
	for _, check := range target.checks {
		value := check + "=" + target.reason
		if existingValue, ok := existing[check]; ok {
			if existingValue != value {
				return nil, fmt.Errorf("%s already has a different disposition", check)
			}
			continue
		}
		highest++
		additions = append(additions, fmt.Sprintf("    checkov.io/skip%d: %q", highest, value))
	}
	if len(additions) == 0 {
		return lines, nil
	}

	return insertLines(lines, insertAt, additions), nil
}

func existingCheckovAnnotations(lines []string, start, end int) (map[string]string, int, error) {
	existing := make(map[string]string)
	highest := 0
	for index := start; index < end; index++ {
		line := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(line, "checkov.io/skip") {
			continue
		}
		key, rawValue, ok := strings.Cut(line, ":")
		if !ok {
			return nil, 0, fmt.Errorf("malformed Checkov annotation %q", line)
		}
		numberText := strings.TrimPrefix(key, "checkov.io/skip")
		number, err := strconv.Atoi(numberText)
		if err != nil || number < 1 {
			return nil, 0, fmt.Errorf("malformed Checkov annotation key %q", key)
		}
		if number > highest {
			highest = number
		}

		var value string
		if err := yaml.Unmarshal([]byte(strings.TrimSpace(rawValue)), &value); err != nil {
			return nil, 0, fmt.Errorf("parse %s: %w", key, err)
		}
		check, _, ok := strings.Cut(value, "=")
		if !ok || check == "" {
			return nil, 0, fmt.Errorf("%s does not name a Checkov check", key)
		}
		if _, duplicate := existing[check]; duplicate {
			return nil, 0, fmt.Errorf("duplicate disposition for %s", check)
		}
		existing[check] = value
	}
	return existing, highest, nil
}

func insertLines(lines []string, index int, additions []string) []string {
	result := make([]string, 0, len(lines)+len(additions))
	result = append(result, lines[:index]...)
	result = append(result, additions...)
	result = append(result, lines[index:]...)
	return result
}
