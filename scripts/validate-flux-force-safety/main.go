package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func resolve(node *yaml.Node) (*yaml.Node, error) {
	seen := map[*yaml.Node]bool{}
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

func field(node *yaml.Node, keys ...string) (*yaml.Node, error) {
	for _, key := range keys {
		var err error
		node, err = resolve(node)
		if err != nil {
			return nil, err
		}
		found := false
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				if node.Content[i].Value == key {
					node = node.Content[i+1]
					found = true
					break
				}
			}
		}
		if !found {
			return &yaml.Node{}, nil
		}
	}
	return resolve(node)
}

func inspect(node *yaml.Node, count *int, ancestors map[*yaml.Node]bool) error {
	node, err := resolve(node)
	if err != nil {
		return err
	}
	if ancestors[node] {
		return errors.New("cyclic YAML collection")
	}
	ancestors[node] = true
	defer delete(ancestors, node)
	if node.Kind == yaml.MappingNode {
		keys := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if node.Content[i].Tag == "!!merge" {
				return errors.New("YAML merge keys are not supported by the replacement guard; make settings explicit")
			}
			if keys[key] {
				return fmt.Errorf("duplicate YAML key %q", key)
			}
			keys[key] = true
		}
		api, err := field(node, "apiVersion")
		if err != nil {
			return err
		}
		kind, err := field(node, "kind")
		if err != nil {
			return err
		}
		if kind.Value == "Kustomization" && strings.HasPrefix(api.Value, "kustomize.toolkit.fluxcd.io/") {
			(*count)++
			force, err := field(node, "spec", "force")
			if err != nil {
				return err
			}
			if force.Kind != 0 {
				if force.Kind != yaml.ScalarNode || force.Tag != "!!bool" {
					return errors.New("Flux force must be a literal boolean")
				}
				var enabled bool
				if err := force.Decode(&enabled); err != nil {
					return err
				}
				if enabled {
					return errors.New("layer-wide force replacement is unsafe; opt individual Jobs in instead")
				}
			}
		}
		if (kind.Value == "PersistentVolumeClaim" && api.Value == "v1") || (kind.Value == "Cluster" && strings.HasPrefix(api.Value, "postgresql.cnpg.io/")) {
			force, err := field(node, "metadata", "annotations", "kustomize.toolkit.fluxcd.io/force")
			if err != nil {
				return err
			}
			if strings.EqualFold(force.Value, "enabled") {
				return errors.New("persistent resource cannot opt into force replacement")
			}
		}
	}
	// Traverse embedded KRO templates and Kubernetes Lists as well as documents.
	// Scalar strings (including scripts and CEL expressions) are never parsed as YAML.
	for _, child := range node.Content {
		if err := inspect(child, count, ancestors); err != nil {
			return err
		}
	}
	return nil
}

func verify(paths ...string) error {
	count := 0
	for _, root := range paths {
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
				if err := inspect(&node, &count, map[*yaml.Node]bool{}); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
			}
		})
		if err != nil {
			return err
		}
	}
	if count == 0 {
		return errors.New("no Flux Kustomization examined")
	}
	return nil
}

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = []string{"k8s"}
	}
	if err := verify(paths...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Verified Flux layers and tenant templates cannot force replacement; persistent resources cannot opt in.")
}
