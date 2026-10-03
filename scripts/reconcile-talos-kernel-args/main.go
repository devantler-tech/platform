// Mirror the post-installer kernel-argument fold audited in KSail v7.193.6, v7.193.8 and v7.194.0,
// configs.go applySchematic/schematicKernelArgs/reconcileFoldedKernelArgs.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const reviewedKSailVersions = "7.193.6, 7.193.8 and 7.194.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 && len(args) != 3 || len(args) == 1 && args[0] != "--check-pins" {
		return fmt.Errorf("usage: reconcile-talos-kernel-args --check-pins | <ksail-config> <control-plane-config> <worker-config>")
	}
	var pins [][]byte
	for _, path := range []string{".github/workflows/ci.yaml", ".github/workflows/cd.yaml", ".github/actions/deploy-prod/action.yml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read deployment pin %s: %w", path, err)
		}
		pins = append(pins, data)
	}
	if err := verifyPins(pins...); err != nil {
		return err
	}
	if len(args) == 1 {
		fmt.Printf("kernel-argument fold matches audited KSail pins (%s)\n", reviewedKSailVersions)
		return nil
	}
	if args[1] == args[2] {
		return fmt.Errorf("control-plane and worker paths must differ")
	}
	inputs := make([][]byte, 3)
	for i, path := range args {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		inputs[i] = data
	}
	cp, worker, err := fold(inputs[0], inputs[1], inputs[2])
	if err != nil {
		return err
	}
	// Parse and reconcile both roles before writing either. Invalid input must
	// never partially modify a rendered configuration.
	for i, data := range [][]byte{cp, worker} {
		if bytes.Equal(data, inputs[i+1]) {
			continue
		}
		if err := os.WriteFile(args[i+1], data, 0o600); err != nil {
			return fmt.Errorf("write rendered config: %w", err)
		}
	}
	fmt.Printf("%s: reconciled source-audited post-installer kernel arguments\n", args[0])
	return nil
}

func documents(data []byte) ([]*yaml.Node, error) {
	d := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*yaml.Node
	for {
		var node yaml.Node
		if err := d.Decode(&node); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("expected mapping documents")
		}
		if err := unambiguous(&node); err != nil {
			return nil, err
		}
		docs = append(docs, &node)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("empty YAML input")
	}
	return docs, nil
}

func unambiguous(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode {
		return fmt.Errorf("aliases are unsupported in the render evidence")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return fmt.Errorf("ambiguous YAML key %q", key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := unambiguous(child); err != nil {
			return err
		}
	}
	return nil
}

func lookup(n *yaml.Node, keys ...string) (*yaml.Node, error) {
	if n != nil && n.Kind == yaml.DocumentNode {
		n = n.Content[0]
	}
	for _, key := range keys {
		if n == nil || n.Tag == "!!null" {
			return nil, nil
		}
		if n.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s parent is not a mapping", key)
		}
		var next *yaml.Node
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				next = n.Content[i+1]
				break
			}
		}
		n = next
	}
	return n, nil
}

func nonblankStrings(n *yaml.Node) ([]string, error) {
	if n == nil || n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected a list of strings")
	}
	var values []string
	for _, child := range n.Content {
		if child.Kind != yaml.ScalarNode || child.Tag != "!!str" {
			return nil, fmt.Errorf("expected string list entries")
		}
		if value := strings.TrimSpace(child.Value); value != "" {
			values = append(values, value)
		}
	}
	return values, nil
}

func fold(config, cp, worker []byte) ([]byte, []byte, error) {
	cluster, err := documents(config)
	if err != nil || len(cluster) != 1 {
		return nil, nil, fmt.Errorf("expected one valid KSail config: %v", err)
	}
	extensions, err := lookup(cluster[0], "spec", "cluster", "talos", "extensions")
	if err != nil {
		return nil, nil, err
	}
	ext, err := nonblankStrings(extensions)
	if err != nil {
		return nil, nil, err
	}
	schematic, err := lookup(cluster[0], "spec", "cluster", "talos", "schematicId")
	if err != nil {
		return nil, nil, err
	}
	explicitSchematic := false
	if schematic != nil && schematic.Tag != "!!null" {
		if schematic.Kind != yaml.ScalarNode || schematic.Tag != "!!str" {
			return nil, nil, fmt.Errorf("schematicId is not a string")
		}
		explicitSchematic = strings.TrimSpace(schematic.Value) != ""
	}
	var roleDocs [2][]*yaml.Node
	var installs [2]*yaml.Node
	var args []string
	seen := map[string]bool{}
	for i, data := range [][]byte{cp, worker} {
		docs, err := documents(data)
		if err != nil {
			return nil, nil, err
		}
		roleDocs[i] = docs
		alphaCount := 0
		for _, doc := range docs {
			version, err := lookup(doc, "version")
			if err != nil {
				return nil, nil, err
			}
			if version == nil || version.Value != "v1alpha1" {
				continue
			}
			alphaCount++
			if alphaCount > 1 {
				return nil, nil, fmt.Errorf("multiple v1alpha1 configs for one role")
			}
			install, err := lookup(doc, "machine", "install")
			if err != nil {
				return nil, nil, err
			}
			if install == nil || install.Tag == "!!null" {
				continue
			}
			if install.Kind != yaml.MappingNode {
				return nil, nil, fmt.Errorf("install is not a mapping")
			}
			installs[i] = install
			kernel, err := lookup(install, "extraKernelArgs")
			if err != nil {
				return nil, nil, err
			}
			values, err := nonblankStrings(kernel)
			if err != nil {
				return nil, nil, err
			}
			uki, err := lookup(install, "grubUseUKICmdline")
			if err != nil {
				return nil, nil, err
			}
			if uki != nil && uki.Tag != "!!null" && uki.Tag != "!!bool" {
				return nil, nil, fmt.Errorf("grubUseUKICmdline is not a boolean")
			}
			for _, arg := range values {
				if !seen[arg] {
					args = append(args, arg)
					seen[arg] = true
				}
			}
		}
	}
	// KSail does not pass extensions to the Talos config manager when an
	// explicit schematic is selected. That path never reaches the fold.
	if explicitSchematic || len(ext) == 0 || len(args) == 0 {
		return cp, worker, nil
	}
	outputs := [2][]byte{cp, worker}
	for i, install := range installs {
		if install == nil {
			continue
		}
		content := []*yaml.Node{}
		for j := 0; j < len(install.Content); j += 2 {
			if key := install.Content[j].Value; key != "extraKernelArgs" && key != "grubUseUKICmdline" {
				content = append(content, install.Content[j], install.Content[j+1])
			}
		}
		install.Content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "grubUseUKICmdline"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		var out bytes.Buffer
		encoder := yaml.NewEncoder(&out)
		encoder.SetIndent(2)
		for _, doc := range roleDocs[i] {
			if err := encoder.Encode(doc); err != nil {
				return nil, nil, err
			}
		}
		if err := encoder.Close(); err != nil {
			return nil, nil, err
		}
		outputs[i] = out.Bytes()
	}
	return outputs[0], outputs[1], nil
}

func verifyPins(inputs ...[]byte) error {
	if len(inputs) == 0 {
		return fmt.Errorf("deployment pin evidence absent")
	}
	selectedVersion := ""
	for _, data := range inputs {
		docs, err := documents(data)
		if err != nil || len(docs) != 1 {
			return fmt.Errorf("invalid deployment pin document: %v", err)
		}
		count := 0
		var visit func(*yaml.Node) error
		visit = func(n *yaml.Node) error {
			if n.Kind == yaml.MappingNode {
				for i := 0; i < len(n.Content); i += 2 {
					if n.Content[i].Value != "KSAIL_VERSION" {
						continue
					}
					v := n.Content[i+1]
					if v.Kind != yaml.ScalarNode || v.Tag != "!!str" || (v.Value != "7.193.6" && v.Value != "7.193.8" && v.Value != "7.194.0") {
						return fmt.Errorf("KSail fold audited at %s; deployment pin %q requires a new source audit", reviewedKSailVersions, v.Value)
					}
					if selectedVersion != "" && v.Value != selectedVersion {
						return fmt.Errorf("divergent KSail deployment pins %q and %q", selectedVersion, v.Value)
					}
					selectedVersion = v.Value
					count++
				}
			}
			for _, child := range n.Content {
				if err := visit(child); err != nil {
					return err
				}
			}
			return nil
		}
		if err := visit(docs[0]); err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("missing explicit KSAIL_VERSION deployment pin")
		}
	}
	return nil
}
