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
		// Kustomize JSON6902 patches are YAML sequences, not policy objects.
		if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
			continue
		}
		var header struct {
			Kind string `yaml:"kind"`
		}
		if err := document.Decode(&header); err != nil {
			return count, fmt.Errorf("%s: %w", source, err)
		}
		if header.Kind != "CiliumNetworkPolicy" && header.Kind != "CiliumClusterwideNetworkPolicy" {
			continue
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
					if port.ServerNames.Kind != yaml.SequenceNode {
						return count, fmt.Errorf("%s %s %s egress[%d] toPorts[%d]: serverNames must be a sequence", source, p.Metadata.Name, label, ei, pi)
					}
					names := make(map[string]bool)
					for _, name := range port.ServerNames.Content {
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
	}
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
