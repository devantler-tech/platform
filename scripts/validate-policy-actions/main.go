package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type policyAction struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		DeprecatedAction    yaml.Node `yaml:"validationFailureAction"`
		DeprecatedOverrides yaml.Node `yaml:"validationFailureActionOverrides"`
		Rules               []struct {
			Name     string    `yaml:"name"`
			Validate yaml.Node `yaml:"validate"`
		} `yaml:"rules"`
	} `yaml:"spec"`
}

func unalias(node *yaml.Node) (*yaml.Node, error) {
	seen := make(map[*yaml.Node]bool)
	for node != nil && node.Kind == yaml.AliasNode {
		if seen[node] {
			return nil, errors.New("cyclic YAML alias")
		}
		seen[node] = true
		node = node.Alias
	}
	if node == nil {
		return nil, errors.New("missing YAML alias target")
	}
	return node, nil
}

func collect(node *yaml.Node, expected, actual map[string]string, ancestors map[*yaml.Node]bool) error {
	node, err := unalias(node)
	if err != nil {
		return err
	}
	if ancestors[node] {
		return errors.New("cyclic policy collection")
	}
	ancestors[node] = true
	defer delete(ancestors, node)
	if node.Kind == 0 {
		return nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		return collect(node.Content[0], expected, actual, ancestors)
	}
	// JSON6902 patch sequences and scalar configuration files are not policies.
	if node.Kind != yaml.MappingNode {
		return nil
	}
	var header struct {
		Kind string `yaml:"kind"`
	}
	if err := node.Decode(&header); err != nil {
		return err
	}
	if header.Kind == "List" || header.Kind == "ClusterPolicyList" {
		var list struct {
			Items yaml.Node `yaml:"items"`
		}
		if err := node.Decode(&list); err != nil {
			return err
		}
		items, err := unalias(&list.Items)
		if err != nil {
			return err
		}
		if items.Kind != yaml.SequenceNode {
			return errors.New("policy collection must contain an items sequence")
		}
		for _, item := range items.Content {
			if err := collect(item, expected, actual, ancestors); err != nil {
				return err
			}
		}
		return nil
	}
	if header.Kind != "ClusterPolicy" {
		return nil
	}
	var policy policyAction
	if err := node.Decode(&policy); err != nil {
		return err
	}
	if policy.Spec.DeprecatedAction.Kind != 0 || policy.Spec.DeprecatedOverrides.Kind != 0 {
		return errors.New("deprecated policy action field remains")
	}
	for _, rule := range policy.Spec.Rules {
		if rule.Validate.Kind == 0 {
			continue
		}
		var validation struct {
			Action    yaml.Node `yaml:"failureAction"`
			Overrides yaml.Node `yaml:"failureActionOverrides"`
		}
		if err := rule.Validate.Decode(&validation); err != nil {
			return err
		}
		key := policy.Metadata.Name + "/" + rule.Name
		if validation.Overrides.Kind != 0 {
			return fmt.Errorf("%s: unreviewed rule failureActionOverrides", key)
		}
		actionNode, err := unalias(&validation.Action)
		if err != nil {
			return err
		}
		action := actionNode.Value
		if actionNode.Kind != yaml.ScalarNode || actionNode.Tag != "!!str" || (action != "Audit" && action != "Enforce") {
			return fmt.Errorf("%s: explicit rule failureAction must be Audit or Enforce", key)
		}
		if _, exists := actual[key]; exists {
			return fmt.Errorf("%s: duplicate validation rule", key)
		}
		actual[key] = action
		want, exists := expected[key]
		if !exists {
			return fmt.Errorf("%s: unreviewed validation rule; add its approved action to the baseline", key)
		}
		if action != want {
			return fmt.Errorf("%s: expected %s, found %s", key, want, action)
		}
	}
	return nil
}

func verify(root string, expected map[string]string) error {
	if len(expected) == 0 {
		return errors.New("empty action baseline")
	}
	for key, action := range expected {
		if action != "Audit" && action != "Enforce" {
			return fmt.Errorf("%s: invalid baseline action", key)
		}
	}
	actual := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		for {
			var node yaml.Node
			err := decoder.Decode(&node)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if err := collect(&node, expected, actual, make(map[*yaml.Node]bool)); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
	})
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, exists := actual[key]; !exists {
			return fmt.Errorf("%s: missing baseline rule", key)
		}
	}
	fmt.Printf("Verified %d explicit validation rule actions against the approved baseline.\n", len(actual))
	return nil
}
func main() {
	root := "k8s"
	baseline := "tests/policy-failure-actions.json"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	if len(os.Args) > 2 {
		baseline = os.Args[2]
	}
	data, err := os.ReadFile(baseline)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var expected map[string]string
	if err := yaml.Unmarshal(data, &expected); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := verify(root, expected); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
