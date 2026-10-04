package main

import (
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type policySpec struct {
	Egress []struct {
		ToFQDNs []struct {
			MatchName string `yaml:"matchName"`
		} `yaml:"toFQDNs"`
		ToPorts []struct {
			ServerNames yaml.Node `yaml:"serverNames"`
		} `yaml:"toPorts"`
	} `yaml:"egress"`
}
type policy struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec  policySpec   `yaml:"spec"`
	Specs []policySpec `yaml:"specs"`
}

func check(input io.Reader, source string) (int, error) {
	decoder := yaml.NewDecoder(input)
	count := 0
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("%s: %w", source, err)
		}
		n, err := checkDocument(&document, source, make(map[*yaml.Node]bool))
		count += n
		if err != nil {
			return count, err
		}
	}
}

func resolveAlias(node *yaml.Node) (*yaml.Node, error) {
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

func checkDocument(document *yaml.Node, source string, ancestors map[*yaml.Node]bool) (int, error) {
	count := 0
	if document.Kind == yaml.DocumentNode {
		if len(document.Content) == 0 {
			return 0, nil
		}
		document = document.Content[0]
	}
	var err error
	document, err = resolveAlias(document)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", source, err)
	}
	// Kustomize JSON6902 patches are sequences, not policy objects.
	if document.Kind != yaml.MappingNode {
		return 0, nil
	}
	if ancestors[document] {
		return 0, fmt.Errorf("%s: cyclic policy List", source)
	}
	ancestors[document] = true
	defer delete(ancestors, document)
	var header struct {
		Kind string `yaml:"kind"`
	}
	if err := document.Decode(&header); err != nil {
		return count, fmt.Errorf("%s: %w", source, err)
	}
	if header.Kind == "List" || header.Kind == "CiliumNetworkPolicyList" || header.Kind == "CiliumClusterwideNetworkPolicyList" {
		var list struct {
			Items []yaml.Node `yaml:"items"`
		}
		if err := document.Decode(&list); err != nil {
			return 0, fmt.Errorf("%s: %w", source, err)
		}
		for i := range list.Items {
			n, err := checkDocument(&list.Items[i], fmt.Sprintf("%s items[%d]", source, i), ancestors)
			count += n
			if err != nil {
				return count, err
			}
		}
		return count, nil
	}
	if header.Kind != "CiliumNetworkPolicy" && header.Kind != "CiliumClusterwideNetworkPolicy" {
		return 0, nil
	}
	var p policy
	if err := document.Decode(&p); err != nil {
		return count, fmt.Errorf("%s: %w", source, err)
	}
	count++
	specs := append([]policySpec{p.Spec}, p.Specs...)
	for si, spec := range specs {
		label := "spec"
		if si > 0 {
			label = fmt.Sprintf("specs[%d]", si-1)
		}
		for ei, egress := range spec.Egress {
			for pi, port := range egress.ToPorts {
				if port.ServerNames.Kind == 0 {
					continue
				} // No SNI pinning is outside this guard's scope.
				serverNames, err := resolveAlias(&port.ServerNames)
				if err != nil {
					return count, fmt.Errorf("%s: %w", source, err)
				}
				if serverNames.Kind != yaml.SequenceNode {
					return count, fmt.Errorf("%s %s %s egress[%d] toPorts[%d]: serverNames must be a sequence", source, p.Metadata.Name, label, ei, pi)
				}
				names := make(map[string]bool)
				for _, entry := range serverNames.Content {
					name, err := resolveAlias(entry)
					if err != nil {
						return count, fmt.Errorf("%s: %w", source, err)
					}
					if name.Kind != yaml.ScalarNode || name.Tag != "!!str" {
						return count, fmt.Errorf("%s %s: serverNames entries must be strings", source, p.Metadata.Name)
					}
					names[name.Value] = true
				}
				for _, fqdn := range egress.ToFQDNs {
					if fqdn.MatchName != "" && !names[fqdn.MatchName] {
						return count, fmt.Errorf("%s %s %s egress[%d] toPorts[%d]: serverNames missing %s", source, p.Metadata.Name, label, ei, pi, fqdn.MatchName)
					}
				}
			}
		}
	}
	return count, nil
}
func run(paths []string) error {
	count := 0
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".yaml" && ext != ".yml" {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			n, checkErr := check(file, path)
			closeErr := file.Close()
			count += n
			return errors.Join(checkErr, closeErr)
		})
		if err != nil {
			return err
		}
	}
	if count == 0 {
		return errors.New("found no Cilium policies; cannot verify SNI coverage")
	}
	fmt.Printf("Verified SNI hostname coverage in %d Cilium policies.\n", count)
	return nil
}
func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = []string{"k8s"}
	}
	if err := run(paths); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
